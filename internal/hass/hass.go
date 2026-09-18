// Package hass publishes a RepeaterTastic site to Home Assistant over MQTT.
//
// This is not the Meshtastic MQTT link: that one speaks the mesh's own protocol to other meshes.
// This speaks Home Assistant's — retained discovery documents, one state topic per device,
// availability — so the repeater and the nodes it hears turn up as devices with sensors and
// nothing has to be configured at the Home Assistant end.
//
// Two things keep it cheap. Every entity of a device reads from that device's single state topic
// through a value_template, so one publish refreshes all of them; and the detail sensors are
// announced with enabled_by_default false, so they exist in Home Assistant without being recorded
// until someone asks for one.
package hass

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	pluginv1 "github.com/ScotMesh/RepeaterTastic/api/plugin/v1"
	"github.com/ScotMesh/RepeaterTastic/sdk"
)

// Settings is the plugin's settings form, as the operator fills it in.
type Settings struct {
	Broker   string `json:"broker"`
	Username string `json:"username"`
	Password string `json:"password"`
	TLS      bool   `json:"tls"`

	DiscoveryPrefix string `json:"discovery_prefix"`
	BaseTopic       string `json:"base_topic"`
	IntervalSeconds int    `json:"interval_seconds"`

	// Radios to publish; empty means every radio.
	Radios []string `json:"radios"`
	// Nodes picked in the GUI. These are published whatever else they do and are never dropped
	// for going quiet: the point of picking one is that it doesn't compete for a slot.
	Nodes []string `json:"nodes"`
	// Scope widens that beyond the picks: picked (default), telemetry, direct, all.
	Scope         string `json:"scope"`
	MaxNodes      int    `json:"max_nodes"`
	StaleAfterHrs int    `json:"stale_after_hours"`

	ChannelMessages bool `json:"channel_messages"`
	DirectMessages  bool `json:"direct_messages"`
}

func (s *Settings) fill() {
	if s.DiscoveryPrefix == "" {
		s.DiscoveryPrefix = "homeassistant"
	}
	if s.BaseTopic == "" {
		s.BaseTopic = "repeatertastic"
	}
	if s.IntervalSeconds <= 0 {
		s.IntervalSeconds = 30
	}
	if s.Scope == "" {
		s.Scope = "picked"
	}
	if s.MaxNodes <= 0 {
		s.MaxNodes = 100
	}
	if s.StaleAfterHrs <= 0 {
		s.StaleAfterHrs = 24
	}
}

func (s *Settings) interval() time.Duration { return time.Duration(s.IntervalSeconds) * time.Second }

// staleWindow is how long a device may be silent before its entities should stop claiming to know
// anything.
func (s *Settings) staleWindow() time.Duration { return time.Duration(s.StaleAfterHrs) * time.Hour }

// publishWait is how long to wait for the broker to take a document that has to arrive.
const publishWait = 10 * time.Second

// connectWait is how long Run waits for a first connection before carrying on and letting paho
// keep trying in the background.
const connectWait = 20 * time.Second

// Link is one run of the plugin: a broker connection and what it has told Home Assistant about.
type Link struct {
	c   *sdk.Client
	set Settings
	// site is the id the topics hang off: the main radio's relay persona.
	site  string
	radio *pluginv1.Radio
	ver   string

	client paho.Client
	// publishFn is how a payload reaches the broker. Tests replace it to see what would have
	// been sent without standing a broker up.
	publishFn func(topic string, v any, retain bool)

	// wake carries a request to publish from a broker callback into Run, so publishing only ever
	// happens on one goroutine and never inside paho's message router.
	wake chan bool // true = announce everything again

	mu        sync.Mutex
	announced map[string]bool // node id (no "!") -> its discovery has been published
	picked    map[string]bool
	nodes     map[string]*pluginv1.Node // node id -> the last we heard of it
	status    map[string]*pluginv1.RadioStatus
	published int
}

func New(c *sdk.Client, set Settings, version string) *Link {
	set.fill()
	picked := map[string]bool{}
	for _, id := range set.Nodes {
		picked[strings.ToLower(strings.TrimSpace(id))] = true
	}
	l := &Link{c: c, set: set, ver: version, announced: map[string]bool{}, picked: picked,
		nodes: map[string]*pluginv1.Node{}, status: map[string]*pluginv1.RadioStatus{},
		wake: make(chan bool, 1)}
	if radios := c.Welcome.GetRadios(); len(radios) > 0 {
		l.radio = radios[0]
		l.site = strings.TrimPrefix(radios[0].GetRelay().GetNodeId(), "!")
	}
	if l.site == "" {
		l.site = "site"
	}
	return l
}

// --- topics ---------------------------------------------------------------

func (l *Link) base() string       { return l.set.BaseTopic + "/" + l.site }
func (l *Link) availTopic() string { return l.base() + "/status" }
func (l *Link) siteState() string  { return l.base() + "/state" }
func (l *Link) nodeState(id string) string {
	return l.base() + "/node/" + strings.TrimPrefix(id, "!") + "/state"
}
func (l *Link) messageTopic(kind, name string) string {
	return l.base() + "/" + kind + "/" + topicSafe(name) + "/message"
}

// Home Assistant topics may not carry +, # or spaces.
func topicSafe(s string) string {
	return strings.NewReplacer("+", "_", "#", "_", "/", "_", " ", "_").Replace(s)
}

// --- run ------------------------------------------------------------------

// Run connects to the broker and keeps Home Assistant up to date until ctx ends.
func (l *Link) Run(ctx context.Context) error {
	if strings.TrimSpace(l.set.Broker) == "" {
		return fmt.Errorf("no broker address; fill in the plugin's settings")
	}
	scheme := "tcp"
	if l.set.TLS {
		scheme = "ssl"
	}
	opts := paho.NewClientOptions().
		AddBroker(fmt.Sprintf("%s://%s", scheme, l.set.Broker)).
		SetClientID("repeatertastic-"+l.site).
		SetUsername(l.set.Username).
		SetPassword(l.set.Password).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(15*time.Second).
		SetKeepAlive(30*time.Second).
		// If this node drops off, its entities should say so rather than hold their last
		// reading for ever.
		SetWill(l.availTopic(), "offline", 1, true)

	opts.OnConnect = func(c paho.Client) {
		l.logf("info", "connected to the broker at %s", l.set.Broker)
		c.Publish(l.availTopic(), 1, true, "online")
		// Home Assistant republishes its birth message when it restarts; that is the moment to
		// announce again, because a broker that lost its retained store would otherwise leave
		// Home Assistant with no entities at all.
		c.Subscribe(l.set.DiscoveryPrefix+"/status", 1, func(_ paho.Client, m paho.Message) {
			if strings.TrimSpace(string(m.Payload())) == "online" {
				l.logf("info", "Home Assistant came back; announcing everything again")
				l.ask(true)
			}
		})
		// A reconnect means the broker may have lost what it was holding, so announce again.
		l.ask(true)
	}
	opts.OnConnectionLost = func(_ paho.Client, err error) {
		l.logf("warn", "lost the broker: %v", err)
		l.report("reconnecting", "warning")
	}
	// Handlers run on paho's router goroutine, and this one only signals, so ordering costs
	// nothing and the router is never held up.
	opts.SetOrderMatters(false)

	l.client = paho.NewClient(opts)
	// With ConnectRetry set, paho never completes this token when the first attempt fails — it
	// sleeps and tries again for ever — so Wait() would block here and the plugin would never
	// reach its loop: no shutdown, no events read, and nothing on the card to say why.
	tok := l.client.Connect()
	select {
	case <-tok.Done():
		if err := tok.Error(); err != nil {
			return fmt.Errorf("connecting to %s: %w", l.set.Broker, err)
		}
	case <-time.After(connectWait):
		l.logf("warn", "no answer from %s yet; still trying in the background", l.set.Broker)
		l.report("connecting to "+l.set.Broker, "warning")
	case <-ctx.Done():
		l.client.Disconnect(0)
		return ctx.Err()
	}
	// However this ends, the broker connection goes with it. Left running, an auto-reconnecting
	// client with the same client id fights the next session for the broker's one slot per id.
	defer l.client.Disconnect(500)

	if err := l.loadStatus(ctx); err != nil {
		l.logf("warn", "could not read the radio status: %v", err)
	}
	if err := l.loadNodes(ctx); err != nil {
		l.logf("warn", "could not read the node list: %v", err)
	}

	tick := time.NewTicker(l.set.interval())
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			// Say goodbye properly rather than leaving the will to fire.
			l.client.Publish(l.availTopic(), 1, true, "offline").WaitTimeout(2 * time.Second)
			return ctx.Err()
		case <-tick.C:
			l.publish(false)
		case again := <-l.wake:
			l.publish(again)
		case msg, ok := <-l.c.Events():
			if !ok {
				return l.c.Err()
			}
			l.handle(msg)
		}
	}
}

// ask asks the run loop to publish. It never blocks: a request already waiting is enough, and one
// that wants everything announced again wins over one that doesn't.
func (l *Link) ask(announceAgain bool) {
	select {
	case l.wake <- announceAgain:
	default:
		if announceAgain {
			select {
			case <-l.wake:
			default:
			}
			select {
			case l.wake <- true:
			default:
			}
		}
	}
}

// handle keeps the local picture up to date from the host's events. The periodic publish is what
// sends it on, so a busy mesh doesn't turn into a busy broker.
func (l *Link) handle(msg *pluginv1.HostMessage) {
	switch {
	case msg.GetNode() != nil:
		if n := msg.GetNode().GetNode(); n != nil && !n.GetLocal() {
			l.mu.Lock()
			l.nodes[strings.ToLower(n.GetNodeId())] = n
			l.mu.Unlock()
		}
	case msg.GetStatusEvent() != nil:
		l.mu.Lock()
		for _, r := range msg.GetStatusEvent().GetRadios() {
			l.status[r.GetRadioId()] = r
		}
		l.mu.Unlock()
	case msg.GetText() != nil:
		l.publishMessage(msg.GetText())
	case msg.GetSettings() != nil:
		// Settings changed: the host restarts the plugin, so there is nothing to do here but
		// say so in the log.
		l.logf("info", "settings changed; restarting to apply them")
	}
}

// loadStatus fills in the radios' figures at startup, rather than waiting for the first status
// event.
func (l *Link) loadStatus(ctx context.Context) error {
	resp, err := l.c.Host.GetStatus(ctx, &pluginv1.GetStatusRequest{})
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range resp.GetRadios() {
		l.status[r.GetRadioId()] = r
	}
	return nil
}

// loadNodes fills the picture in at startup, so the first publish isn't empty while waiting for
// each node to be heard again.
func (l *Link) loadNodes(ctx context.Context) error {
	resp, err := l.c.Host.ListNodes(ctx, &pluginv1.ListNodesRequest{})
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, n := range resp.GetNodes() {
		if !n.GetLocal() {
			l.nodes[strings.ToLower(n.GetNodeId())] = n
		}
	}
	return nil
}

// wantRadio reports whether a radio is one the operator chose to publish.
func (l *Link) wantRadio(id string) bool {
	if len(l.set.Radios) == 0 {
		return true
	}
	for _, r := range l.set.Radios {
		if r == id {
			return true
		}
	}
	return false
}

// report tells the GUI what the plugin is doing, on the plugin's card.
func (l *Link) report(summary, state string) {
	if l.c == nil {
		return
	}
	l.mu.Lock()
	n := l.published
	l.mu.Unlock()
	_ = l.c.Status(summary, state, map[string]string{
		"Broker": l.set.Broker,
		"Nodes":  fmt.Sprint(n),
	})
}

func (l *Link) pub(topic string, v any, retain bool) {
	if l.publishFn != nil {
		l.publishFn(topic, v, retain)
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		l.logf("warn", "could not encode %s: %v", topic, err)
		return
	}
	l.client.Publish(topic, 0, retain, b)
}

// pubSure publishes something that has to arrive, and reports whether it did. paho drops a QoS 0
// publish while it is reconnecting and still reports success, so discovery goes out at QoS 1 and
// the result is waited for.
func (l *Link) pubSure(topic string, v any) bool {
	if l.publishFn != nil {
		l.publishFn(topic, v, true)
		return true
	}
	b, err := json.Marshal(v)
	if err != nil {
		l.logf("warn", "could not encode %s: %v", topic, err)
		return false
	}
	tok := l.client.Publish(topic, 1, true, b)
	if !tok.WaitTimeout(publishWait) {
		l.logf("warn", "the broker has not taken %s yet", topic)
		return false
	}
	if err := tok.Error(); err != nil {
		l.logf("warn", "publishing %s: %v", topic, err)
		return false
	}
	return true
}

// clear publishes an empty retained payload, which is how a topic is taken back: Home Assistant
// drops the entity, and a reader that connects later isn't handed a stale value.
func (l *Link) clear(topic string) {
	if l.publishFn != nil {
		l.publishFn(topic, nil, true)
		return
	}
	l.client.Publish(topic, 1, true, []byte{})
}

// logf writes to the plugin's log, and does nothing in a test with no host behind it.
func (l *Link) logf(level, format string, args ...any) {
	if l.c != nil {
		_ = l.c.Log(level, format, args...)
	}
}
