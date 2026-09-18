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
func (l *Link) publish() {
	if l.client == nil || !l.client.IsConnected() {
		return
	}
	l.publishNoBroker()
}

// publishNoBroker is publish once the broker has been checked, so tests can drive it with
// publishFn standing in for a connection.
func (l *Link) publishNoBroker() {
	// With no figures yet, say nothing rather than publish zeros: a zero counter reads as a
	// counter reset in Home Assistant's statistics, and a zeroed radio reads as one that is down.
	if site := l.siteStateJSON(); site != nil {
		l.announceSite()
		l.pub(l.siteState(), site, true)
	}

	nodes := l.nodesToPublish()
	live := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		live[strings.TrimPrefix(strings.ToLower(n.GetNodeId()), "!")] = true
		l.announceNode(n)
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
	defer l.mu.Unlock()

	var (
		airtime, chanUtil, dutyLimit float64
		noise                        int32
		radios                       int
		connected                    = true
		rx, tx, dupe, undec          uint64
		ackOK, ackFail               uint64
		queue, heard                 uint32
		uptime                       int64
	)
	ids := make([]string, 0, len(l.status))
	for id := range l.status {
		ids = append(ids, id)
	}
	sort.Strings(ids) // a stable order, so the noise floor below is deterministic
	for _, id := range ids {
		r := l.status[id]
		if !l.wantRadio(id) {
			continue
		}
		radios++
		// The busiest radio is the one worth alarming on, so take the worst of each figure
		// rather than an average that hides it.
		airtime = max(airtime, r.GetAirtimeTxPct())
		chanUtil = max(chanUtil, r.GetChannelUtilPct())
		dutyLimit = max(dutyLimit, r.GetDutyLimitPct())
		if noise == 0 || r.GetNoiseFloorDbm() > noise {
			noise = r.GetNoiseFloorDbm()
		}
		connected = connected && r.GetConnected()
		rx += r.GetRx()
		tx += r.GetTx()
		dupe += r.GetRxDupe()
		undec += r.GetRxUndecryptable()
		ackOK += r.GetAckOk()
		ackFail += r.GetAckFail()
		queue += r.GetQueue()
		heard = max(heard, r.GetNodesHeard())
		uptime = max(uptime, r.GetUptimeS())
	}
	if radios == 0 {
		return nil
	}

	ackPct := 0.0
	if acks := ackOK + ackFail; acks > 0 {
		ackPct = float64(ackOK) / float64(acks) * 100
	}
	return map[string]any{
		"airtime_tx_pct":   round1(airtime),
		"channel_util_pct": round1(chanUtil),
		"noise_floor_dbm":  noise,
		"rx":               rx,
		"tx":               tx,
		"rx_dupe":          dupe,
		"rx_undecryptable": undec,
		"ack_pct":          round1(ackPct),
		"nodes_heard":      heard,
		"queue":            queue,
		"connected":        onOff(connected),
		"duty_exceeded":    onOff(dutyLimit > 0 && airtime > dutyLimit),
		"duty_limit_pct":   round1(dutyLimit),
		"uptime_s":         uptime,
	}
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
		if !l.wantRadio(n.GetRadioId()) {
			continue
		}
		if picks[strings.ToLower(n.GetNodeId())] {
			picked = append(picked, n)
			continue
		}
		if l.set.Scope == "picked" {
			continue
		}
		heard := n.GetLastHeardMs()
		if heard == 0 || now.Sub(time.UnixMilli(heard)) > l.set.staleWindow() {
			continue
		}
		switch l.set.Scope {
		case "direct":
			if n.GetHopsAway() != 0 {
				continue
			}
		case "telemetry":
			if len(n.GetDeviceMetrics()) == 0 {
				continue
			}
		}
		rest = append(rest, n)
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

// nodeStateJSON is one node's readings. A field it has never reported is left out rather than sent
// as zero, so Home Assistant shows "unknown" instead of a confident wrong number.
func nodeStateJSON(n *pluginv1.Node) map[string]any {
	state := map[string]any{}
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
	}
	if n.GetSnr() != 0 {
		state["snr"] = round1(float64(n.GetSnr()))
	}
	if n.GetRssi() != 0 {
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
