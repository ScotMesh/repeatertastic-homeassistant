package hass

import (
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	meshtastic "github.com/ScotMesh/RepeaterTastic/api/meshtastic"
	pluginv1 "github.com/ScotMesh/RepeaterTastic/api/plugin/v1"
)

// publish sends the site's state and every node worth publishing. Everything Home Assistant reads
// comes from these few retained topics, so a reader that connects later still sees current values.
func (l *Link) publish(announceAgain bool) {
	if l.client == nil || !l.client.IsConnected() {
		return
	}
	l.publishNoBroker(announceAgain)
}

// publishNoBroker is publish once the broker has been checked, so tests can drive it with
// publishFn standing in for a connection. It runs only on the Run loop, so a pass never overlaps
// another and the announce/forget bookkeeping stays consistent.
func (l *Link) publishNoBroker(announceAgain bool) {

	// With no figures yet, say nothing rather than publish zeros: a zero counter reads as a
	// counter reset in Home Assistant's statistics, and a zeroed radio reads as one that is down.
	if site := l.siteStateJSON(); site != nil {
		l.announceSite(announceAgain)
		l.pub(l.siteState(), site, true)
	}

	nodes := l.nodesToPublish()
	live := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		live[strings.TrimPrefix(strings.ToLower(n.GetNodeId()), "!")] = true
		l.announceNode(n, announceAgain)
		l.pub(l.nodeState(n.GetNodeId()), nodeStateJSON(n), true)
	}

	// A node that has dropped out of the list — gone quiet past the stale window, unpicked, or
	// pushed out by the ceiling — has its entities taken back. Discovery is retained, so without
	// this it would sit in Home Assistant for ever showing readings that stopped being true.
	l.mu.Lock()
	var gone []string
	for id := range l.announced {
		if id != l.site && !live[id] {
			gone = append(gone, id)
		}
	}
	l.published = len(nodes)
	l.mu.Unlock()
	for _, id := range gone {
		l.forget(id)
	}
	l.report(summaryLine(len(nodes)), "ok")
}

func summaryLine(n int) string {
	switch n {
	case 0:
		return "Publishing the site"
	case 1:
		return "Publishing the site and 1 node"
	default:
		return "Publishing the site and " + itoa(n) + " nodes"
	}
}

// siteStateJSON is every figure the site's entities read, added up across the radios being
// published. Counters are totals since the daemon started, which is what Home Assistant's
// total_increasing expects. It returns nil when no radio has reported yet, because publishing
// zeros then would be a lie the statistics remember.
func (l *Link) siteStateJSON() map[string]any {
	l.mu.Lock()
	ids := make([]string, 0, len(l.status))
	for id := range l.status {
		if l.wantRadio(id) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids) // a stable order, so the figures below are deterministic
	radios := make([]*pluginv1.RadioStatus, 0, len(ids))
	for _, id := range ids {
		radios = append(radios, l.status[id])
	}
	l.mu.Unlock()

	if len(radios) == 0 {
		return nil
	}
	t := total(radios)
	ackPct := 0.0
	if acks := t.ackOK + t.ackFail; acks > 0 {
		ackPct = float64(t.ackOK) / float64(acks) * 100
	}
	out := map[string]any{
		"airtime_tx_pct":   round1(t.airtime),
		"channel_util_pct": round1(t.chanUtil),
		"noise_floor_dbm":  nil,
		"rx":               t.rx,
		"tx":               t.tx,
		"rx_dupe":          t.dupe,
		"rx_undecryptable": t.undec,
		"ack_pct":          round1(ackPct),
		"nodes_heard":      t.heard,
		"queue":            t.queue,
		"connected":        onOff(t.connected),
		"duty_exceeded":    onOff(t.overDuty),
		"duty_limit_pct":   round1(t.dutyLimit),
		"uptime_s":         t.uptime,
	}
	if t.haveNoise {
		out["noise_floor_dbm"] = t.noise
	}
	return out
}

// totals is the site's figures across the radios being published.
type totals struct {
	airtime, chanUtil, dutyLimit float64
	noise                        int32
	haveNoise, overDuty          bool
	connected                    bool
	rx, tx, dupe, undec          uint64
	ackOK, ackFail               uint64
	queue, heard                 uint32
	uptime                       int64
}

// total adds the radios up. The worst of each figure is what is worth alarming on, so a busy
// radio is not averaged away by a quiet one.
func total(radios []*pluginv1.RadioStatus) totals {
	t := totals{connected: true}
	for _, r := range radios {
		t.airtime = max(t.airtime, r.GetAirtimeTxPct())
		t.chanUtil = max(t.chanUtil, r.GetChannelUtilPct())
		t.dutyLimit = max(t.dutyLimit, r.GetDutyLimitPct())
		// Each radio is over its own limit or it isn't. Comparing the busiest radio's airtime
		// against another radio's limit reports a breach nobody is committing.
		if limit := r.GetDutyLimitPct(); limit > 0 && r.GetAirtimeTxPct() > limit {
			t.overDuty = true
		}
		// A noise floor is negative dBm, and zero means "not measured yet" rather than a very
		// loud band, so a radio that has only just come up must not win.
		if n := r.GetNoiseFloorDbm(); n != 0 && (!t.haveNoise || n > t.noise) {
			t.noise, t.haveNoise = n, true
		}
		t.connected = t.connected && r.GetConnected()
		t.rx += r.GetRx()
		t.tx += r.GetTx()
		t.dupe += r.GetRxDupe()
		t.undec += r.GetRxUndecryptable()
		t.ackOK += r.GetAckOk()
		t.ackFail += r.GetAckFail()
		t.queue += r.GetQueue()
		t.heard = max(t.heard, r.GetNodesHeard())
		t.uptime = max(t.uptime, r.GetUptimeS())
	}
	return t
}

// nodesToPublish applies the scope, the picks and the ceiling. Picked nodes come first and are
// never dropped by the ceiling: the point of picking one is that it doesn't compete with a busy
// mesh for a slot.
func (l *Link) nodesToPublish() []*pluginv1.Node {
	now := time.Now()
	l.mu.Lock()
	all := make([]*pluginv1.Node, 0, len(l.nodes))
	for _, n := range l.nodes {
		all = append(all, n)
	}
	picks := l.picked
	l.mu.Unlock()

	var picked, rest []*pluginv1.Node
	for _, n := range all {
		switch {
		case !l.wantRadio(n.GetRadioId()):
		case picks[strings.ToLower(n.GetNodeId())]:
			picked = append(picked, n)
		case l.inScope(n, now):
			rest = append(rest, n)
		}
	}
	byHeard := func(s []*pluginv1.Node) {
		sort.Slice(s, func(i, j int) bool { return s[i].GetLastHeardMs() > s[j].GetLastHeardMs() })
	}
	byHeard(picked)
	byHeard(rest)

	// The ceiling is on the nodes nobody asked for by name; a pick is a deliberate choice and
	// isn't going to be pushed out by a chatty stranger.
	out := picked
	for i, n := range rest {
		if i >= l.set.MaxNodes {
			break
		}
		out = append(out, n)
	}
	return out
}

// inScope reports whether an unpicked node is one the "Also publish" setting asks for.
func (l *Link) inScope(n *pluginv1.Node, now time.Time) bool {
	if l.set.Scope == "picked" {
		return false
	}
	heard := n.GetLastHeardMs()
	if heard == 0 || now.Sub(time.UnixMilli(heard)) > l.set.staleWindow() {
		return false
	}
	switch l.set.Scope {
	case "direct":
		return n.GetHopsAway() == 0
	case "telemetry":
		return len(n.GetDeviceMetrics()) > 0
	default:
		return true
	}
}

// nodeStateJSON is one node's readings. A field the node has stopped reporting is sent as null
// rather than left out: Home Assistant ignores an empty render and keeps the last value, so an
// omitted battery would sit at its final reading for ever and a low-battery automation would
// never fire. Null renders as "None", which Home Assistant reads as unknown.
func nodeStateJSON(n *pluginv1.Node) map[string]any {
	state := map[string]any{
		"battery": nil, "powered": nil, "voltage": nil,
		"channel_util_pct": nil, "air_util_tx_pct": nil,
		"last_heard": nil, "snr": nil, "rssi": nil, "hops": nil,
	}
	if m := deviceMetrics(n); m != nil {
		// Meshtastic reports 101 for a node running on external power. Home Assistant would draw
		// that as a 101% battery, so say "powered" instead and keep the percentage honest.
		batt := int(m.GetBatteryLevel())
		state["powered"] = onOff(batt > 100)
		if batt > 100 {
			batt = 100
		}
		if batt > 0 {
			state["battery"] = batt
		}
		if v := m.GetVoltage(); v > 0 {
			state["voltage"] = round2(float64(v))
		}
		if u := m.GetChannelUtilization(); u > 0 {
			state["channel_util_pct"] = round1(float64(u))
		}
		if a := m.GetAirUtilTx(); a > 0 {
			state["air_util_tx_pct"] = round1(float64(a))
		}
	}
	if ms := n.GetLastHeardMs(); ms > 0 {
		state["last_heard"] = time.UnixMilli(ms).UTC().Format(time.RFC3339)
		// Once a node has been heard, its signal figures are real readings — including a zero,
		// which is an ordinary SNR and was previously thrown away as if it meant "unknown".
		state["snr"] = round1(float64(n.GetSnr()))
		state["rssi"] = n.GetRssi()
	}
	if h := n.GetHopsAway(); h >= 0 {
		state["hops"] = h
	}
	return state
}

// publishMessage sends one received message as an event. It is deliberately not retained: a
// retained message would be redelivered on every reconnect and fire every automation listening to
// it again.
func (l *Link) publishMessage(m *pluginv1.TextMessageEvent) {
	if m.GetDirection() != "in" || m.GetText() == "" {
		return
	}
	if m.GetDirect() && !l.set.DirectMessages {
		return
	}
	if !m.GetDirect() && !l.set.ChannelMessages {
		return
	}
	if !l.wantRadio(m.GetRadioId()) {
		return
	}
	from := nodeID(m.GetFrom())
	payload := map[string]any{
		"from": from, "to": nodeID(m.GetTo()), "text": m.GetText(),
		"channel": m.GetChannel(), "identity": m.GetIdentityNodeId(),
		"radio": m.GetRadioId(), "direct": m.GetDirect(),
		"time": time.UnixMilli(m.GetTimeMs()).UTC().Format(time.RFC3339),
	}
	l.mu.Lock()
	n := l.nodes[strings.ToLower(from)]
	l.mu.Unlock()
	if u := user(n); u != nil && u.GetLongName() != "" {
		payload["from_name"] = u.GetLongName()
	}

	kind, name := "channel", itoa(int(m.GetChannel()))
	if m.GetDirect() {
		kind, name = "identity", strings.TrimPrefix(m.GetIdentityNodeId(), "!")
	}
	l.pub(l.messageTopic(kind, name), payload, false)
}

// --- the bits of a Node that arrive as protobuf bytes ----------------------

func user(n *pluginv1.Node) *meshtastic.User {
	if n == nil || len(n.GetUser()) == 0 {
		return nil
	}
	var u meshtastic.User
	if proto.Unmarshal(n.GetUser(), &u) != nil {
		return nil
	}
	return &u
}

func deviceMetrics(n *pluginv1.Node) *meshtastic.DeviceMetrics {
	if n == nil || len(n.GetDeviceMetrics()) == 0 {
		return nil
	}
	var m meshtastic.DeviceMetrics
	if proto.Unmarshal(n.GetDeviceMetrics(), &m) != nil {
		return nil
	}
	return &m
}

// --- helpers --------------------------------------------------------------

func onOff(b bool) string {
	if b {
		return "ON"
	}
	return "OFF"
}

func round1(f float64) float64 { return float64(int(f*10+0.5)) / 10 }
func round2(f float64) float64 { return float64(int(f*100+0.5)) / 100 }

func nodeID(num uint32) string {
	const hex = "0123456789abcdef"
	b := []byte{'!', 0, 0, 0, 0, 0, 0, 0, 0}
	for i := 0; i < 8; i++ {
		b[8-i] = hex[(num>>(4*i))&0xf]
	}
	return string(b)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
