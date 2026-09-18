package hass

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	meshtastic "github.com/ScotMesh/RepeaterTastic/api/meshtastic"
	pluginv1 "github.com/ScotMesh/RepeaterTastic/api/plugin/v1"
)

// testLink is a Link with no broker behind it, for the parts that only decide what to publish.
func testLink(t *testing.T, set Settings, nodes ...*pluginv1.Node) *Link {
	t.Helper()
	set.fill()
	l := &Link{set: set, site: "a1c40e07", ver: "1.0.0", announced: map[string]bool{},
		picked: map[string]bool{}, nodes: map[string]*pluginv1.Node{},
		status: map[string]*pluginv1.RadioStatus{}}
	for _, id := range set.Nodes {
		l.picked[strings.ToLower(id)] = true
	}
	for _, n := range nodes {
		l.nodes[strings.ToLower(n.GetNodeId())] = n
	}
	return l
}

func node(id string, heardAgo time.Duration, opts ...func(*pluginv1.Node)) *pluginv1.Node {
	n := &pluginv1.Node{NodeId: id, RadioId: "main", HopsAway: 1,
		LastHeardMs: time.Now().Add(-heardAgo).UnixMilli()}
	for _, o := range opts {
		o(n)
	}
	return n
}

func withBattery(pct uint32) func(*pluginv1.Node) {
	return func(n *pluginv1.Node) {
		b, _ := proto.Marshal(&meshtastic.DeviceMetrics{BatteryLevel: &pct})
		n.DeviceMetrics = b
	}
}

func withName(long string) func(*pluginv1.Node) {
	return func(n *pluginv1.Node) {
		b, _ := proto.Marshal(&meshtastic.User{LongName: long})
		n.User = b
	}
}

func direct() func(*pluginv1.Node) { return func(n *pluginv1.Node) { n.HopsAway = 0 } }

func ids(nodes []*pluginv1.Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.GetNodeId())
	}
	return out
}

func TestPickedNodesAreAlwaysPublished(t *testing.T) {
	// A picked node that has been quiet for a week is still published: the point of picking one
	// is that it doesn't have to keep proving itself.
	quiet := node("!aaaa0001", 7*24*time.Hour)
	recent := node("!bbbb0002", time.Minute)
	l := testLink(t, Settings{Nodes: []string{"!aaaa0001"}}, quiet, recent)

	got := ids(l.nodesToPublish())
	if len(got) != 1 || got[0] != "!aaaa0001" {
		t.Fatalf("scope picked should publish only the pick, got %v", got)
	}
}

func TestScopeWidensTheList(t *testing.T) {
	near := node("!dddd0004", time.Minute, direct())
	far := node("!eeee0005", time.Minute)
	withBatt := node("!ffff0006", time.Minute, withBattery(80))
	stale := node("!99990007", 48*time.Hour, direct())

	cases := map[string][]string{
		"picked":    {},
		"direct":    {"!dddd0004"},
		"telemetry": {"!ffff0006"},
		"all":       {"!9999f007"}, // placeholder, replaced below
	}
	for scope, want := range cases {
		t.Run(scope, func(t *testing.T) {
			l := testLink(t, Settings{Scope: scope}, near, far, withBatt, stale)
			got := ids(l.nodesToPublish())
			if scope == "all" {
				// Everything heard inside the stale window, and not the one that wasn't.
				if len(got) != 3 {
					t.Fatalf("wanted the three recent nodes, got %v", got)
				}
				for _, id := range got {
					if id == "!99990007" {
						t.Error("a node silent for 48h was published under a 24h window")
					}
				}
				return
			}
			if len(got) != len(want) {
				t.Fatalf("wanted %v, got %v", want, got)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("wanted %v, got %v", want, got)
				}
			}
		})
	}
}

func TestTheCeilingDoesNotApplyToPicks(t *testing.T) {
	nodes := []*pluginv1.Node{
		node("!aaaa0001", time.Minute), node("!bbbb0002", 2*time.Minute),
		node("!cccc0003", 3*time.Minute), node("!dddd0004", 4*time.Minute),
	}
	l := testLink(t, Settings{Scope: "all", MaxNodes: 2, Nodes: []string{"!dddd0004"}}, nodes...)

	got := ids(l.nodesToPublish())
	if len(got) != 3 {
		t.Fatalf("wanted the pick plus two others, got %v", got)
	}
	if got[0] != "!dddd0004" {
		t.Errorf("the pick should come first, got %v", got)
	}
	// The two others are the most recently heard.
	if got[1] != "!aaaa0001" || got[2] != "!bbbb0002" {
		t.Errorf("the ceiling should keep the most recent, got %v", got)
	}
}

func TestExternalPowerIsNotA101PercentBattery(t *testing.T) {
	// Meshtastic reports 101 for a node on external power.
	state := nodeStateJSON(node("!aaaa0001", time.Minute, withBattery(101)))
	if state["powered"] != "ON" {
		t.Errorf("101 should read as powered, got %v", state["powered"])
	}
	if state["battery"] != 100 {
		t.Errorf("the battery should be capped at 100, got %v", state["battery"])
	}

	onBattery := nodeStateJSON(node("!bbbb0002", time.Minute, withBattery(42)))
	if onBattery["powered"] != "OFF" || onBattery["battery"] != 42 {
		t.Errorf("a node on battery: %v", onBattery)
	}
}

func TestAFieldNeverReportedIsLeftOut(t *testing.T) {
	// Home Assistant shows "unknown" for a missing field, which is honest. A zero would be a
	// confident wrong reading.
	state := nodeStateJSON(&pluginv1.Node{NodeId: "!aaaa0001", HopsAway: -1})
	for _, k := range []string{"battery", "voltage", "snr", "rssi", "hops", "last_heard"} {
		if _, ok := state[k]; ok {
			t.Errorf("%s was reported for a node that has never sent one: %v", k, state[k])
		}
	}
}

func TestSiteStateTakesTheWorstOfEachRadio(t *testing.T) {
	l := testLink(t, Settings{})
	l.status["main"] = &pluginv1.RadioStatus{RadioId: "main", Connected: true,
		AirtimeTxPct: 3, ChannelUtilPct: 10, DutyLimitPct: 10, NoiseFloorDbm: -110,
		Rx: 100, Tx: 10, AckOk: 8, AckFail: 2, NodesHeard: 20}
	l.status["mf"] = &pluginv1.RadioStatus{RadioId: "mf", Connected: true,
		AirtimeTxPct: 12, ChannelUtilPct: 4, DutyLimitPct: 10, NoiseFloorDbm: -95,
		Rx: 50, Tx: 5, AckOk: 2, AckFail: 8, NodesHeard: 7}

	s := l.siteStateJSON()
	if s["airtime_tx_pct"] != 12.0 {
		t.Errorf("airtime should be the busiest radio's, got %v", s["airtime_tx_pct"])
	}
	if s["noise_floor_dbm"] != int32(-95) {
		t.Errorf("the noise floor should be the worst, got %v", s["noise_floor_dbm"])
	}
	if s["rx"] != uint64(150) {
		t.Errorf("counters should add up, got %v", s["rx"])
	}
	if s["ack_pct"] != 50.0 {
		t.Errorf("ack percent across both radios should be 50, got %v", s["ack_pct"])
	}
	if s["duty_exceeded"] != "ON" {
		t.Errorf("12%% against a 10%% limit is exceeded, got %v", s["duty_exceeded"])
	}
	if s["connected"] != "ON" {
		t.Errorf("both radios are up, got %v", s["connected"])
	}

	// One radio down means the site is not fully on air, and that should show.
	l.status["mf"].Connected = false
	if s := l.siteStateJSON(); s["connected"] != "OFF" {
		t.Errorf("a radio being down should show, got %v", s["connected"])
	}
}

func TestOnlyTheChosenRadiosCount(t *testing.T) {
	l := testLink(t, Settings{Radios: []string{"main"}},
		node("!aaaa0001", time.Minute), &pluginv1.Node{NodeId: "!bbbb0002", RadioId: "mf",
			LastHeardMs: time.Now().UnixMilli()})
	l.set.Scope = "all"
	l.status["main"] = &pluginv1.RadioStatus{RadioId: "main", Connected: true, Rx: 10}
	l.status["mf"] = &pluginv1.RadioStatus{RadioId: "mf", Connected: true, Rx: 999}

	if got := ids(l.nodesToPublish()); len(got) != 1 || got[0] != "!aaaa0001" {
		t.Errorf("a node on an unchosen radio was published: %v", got)
	}
	if s := l.siteStateJSON(); s["rx"] != uint64(10) {
		t.Errorf("an unchosen radio's counters were added in: %v", s["rx"])
	}
}

// captured is what a Link would have published, without a broker.
type captured struct {
	topic   string
	payload map[string]any
	retain  bool
}

func capture(l *Link) *[]captured {
	var out []captured
	l.publishFn = func(topic string, v any, retain bool) {
		b, _ := json.Marshal(v)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		out = append(out, captured{topic, m, retain})
	}
	return &out
}

func TestDiscoveryDocumentsAreRetainedAndPointAtTheState(t *testing.T) {
	l := testLink(t, Settings{}, node("!aaaa0001", time.Minute, withName("Shed")))
	got := capture(l)
	l.announceNode(l.nodes["!aaaa0001"])

	if len(*got) != len(nodeEntities) {
		t.Fatalf("wanted one document per entity, got %d", len(*got))
	}
	var battery *captured
	for i := range *got {
		c := &(*got)[i]
		if !c.retain {
			t.Errorf("%s was not retained; Home Assistant would lose it on a restart", c.topic)
		}
		if !strings.HasPrefix(c.topic, "homeassistant/") || !strings.HasSuffix(c.topic, "/config") {
			t.Errorf("unexpected discovery topic %s", c.topic)
		}
		if c.payload["unique_id"] == "rt_node_aaaa0001_battery" {
			battery = c
		}
	}
	if battery == nil {
		t.Fatal("no battery entity was announced")
	}
	if battery.payload["state_topic"] != l.nodeState("!aaaa0001") {
		t.Errorf("the battery reads from %v, not the node's state topic", battery.payload["state_topic"])
	}
	if battery.payload["value_template"] != "{{ value_json.battery }}" {
		t.Errorf("value_template is %v", battery.payload["value_template"])
	}
	if battery.payload["availability_topic"] != l.availTopic() {
		t.Error("an entity with no availability topic keeps its last reading for ever")
	}
	dev, _ := battery.payload["device"].(map[string]any)
	if dev["name"] != "Shed" {
		t.Errorf("the device should carry the node's name, got %v", dev["name"])
	}
	if dev["via_device"] != "rt_a1c40e07" {
		t.Errorf("a node should hang off the site, got %v", dev["via_device"])
	}

	// Announcing again does nothing: discovery is retained, so repeating it is pure broker noise.
	before := len(*got)
	l.announceNode(l.nodes["!aaaa0001"])
	if len(*got) != before {
		t.Error("the same node was announced twice")
	}
}

func TestForgettingANodeClearsItsEntities(t *testing.T) {
	l := testLink(t, Settings{}, node("!aaaa0001", time.Minute))
	got := capture(l)
	l.announceNode(l.nodes["!aaaa0001"])
	*got = nil // the announcements are not what this test is about
	l.forget("!aaaa0001")

	empty := 0
	for _, c := range *got {
		if c.payload == nil && c.retain {
			empty++
		}
	}
	if empty != len(nodeEntities)+1 {
		t.Fatalf("wanted an empty retained payload per entity plus the state topic, got %d of %d", empty, len(*got))
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.announced["aaaa0001"] {
		t.Error("a forgotten node should be announced again if it comes back")
	}
}

func TestMessagesAreNotRetained(t *testing.T) {
	l := testLink(t, Settings{ChannelMessages: true}, node("!aaaa0001", time.Minute, withName("Shed")))
	got := capture(l)
	l.publishMessage(&pluginv1.TextMessageEvent{
		RadioId: "main", Direction: "in", Text: "hello", From: 0xaaaa0001,
		Channel: 2, TimeMs: time.Now().UnixMilli(),
	})
	if len(*got) != 1 {
		t.Fatalf("wanted one publish, got %d", len(*got))
	}
	c := (*got)[0]
	if c.retain {
		t.Error("a retained message is redelivered on every reconnect and fires every automation again")
	}
	if c.payload["text"] != "hello" || c.payload["from_name"] != "Shed" {
		t.Errorf("payload: %v", c.payload)
	}

	// Direct messages are a separate switch, off here.
	l.publishMessage(&pluginv1.TextMessageEvent{RadioId: "main", Direction: "in", Text: "psst", Direct: true})
	if len(*got) != 1 {
		t.Error("a direct message went out with direct messages turned off")
	}
	// So is a message this site sent.
	l.publishMessage(&pluginv1.TextMessageEvent{RadioId: "main", Direction: "out", Text: "mine"})
	if len(*got) != 1 {
		t.Error("an outgoing message was published as if it were heard")
	}
}

func TestNodeIDFormatting(t *testing.T) {
	for num, want := range map[uint32]string{0xa1c40e07: "!a1c40e07", 1: "!00000001", 0: "!00000000"} {
		if got := nodeID(num); got != want {
			t.Errorf("nodeID(%#x) = %s, wanted %s", num, got, want)
		}
	}
}

func TestNoFiguresYetMeansNoSiteState(t *testing.T) {
	// Before the first status arrives there is nothing true to say about the radios. Publishing
	// zeros would tell Home Assistant the counters reset and the radio is down, and its
	// statistics would remember both.
	l := testLink(t, Settings{Scope: "all"}, node("!aaaa0001", time.Minute))
	if s := l.siteStateJSON(); s != nil {
		t.Fatalf("wanted no site state before any radio has reported, got %v", s)
	}

	got := capture(l)
	l.publishNoBroker()
	for _, c := range *got {
		if c.topic == l.siteState() {
			t.Error("the site state was published with no figures behind it")
		}
		if strings.Contains(c.topic, "/rt_"+l.site+"/") {
			t.Error("the site's entities were announced before it had anything to report")
		}
	}
	// The nodes are still published: those readings are real.
	if len(*got) == 0 {
		t.Error("nothing at all was published")
	}

	// Once a radio reports, the site turns up.
	l.status["main"] = &pluginv1.RadioStatus{RadioId: "main", Connected: true, Rx: 5}
	*got = nil
	l.publishNoBroker()
	found := false
	for _, c := range *got {
		if c.topic == l.siteState() {
			found = true
			if c.payload["rx"] != 5.0 {
				t.Errorf("rx is %v", c.payload["rx"])
			}
		}
	}
	if !found {
		t.Error("the site state was not published once a radio had reported")
	}
}
