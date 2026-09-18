package hass

import (
	"fmt"
	"strings"

	pluginv1 "github.com/ScotMesh/RepeaterTastic/api/plugin/v1"
)

// A discovery document tells Home Assistant an entity exists, where to read it and how to draw
// it. They are retained, so Home Assistant finds them whenever it starts — and an entity is
// removed by publishing an empty retained payload over its config topic, which is the only way to
// make Home Assistant forget a node that has gone for good.

type entity struct {
	Component string // sensor, binary_sensor
	Key       string // object id, unique within the device
	Name      string
	Field     string // key in the device's state JSON
	Unit      string
	Class     string // device_class
	StateCls  string // state_class
	Icon      string
	Category  string // "diagnostic" puts it below the fold in Home Assistant
	Default   bool   // enabled_by_default
}

// The site's own entities. Everything here reads from one state topic.
var siteEntities = []entity{
	{Component: "sensor", Key: "airtime", Name: "Airtime", Field: "airtime_tx_pct", Unit: "%",
		StateCls: "measurement", Icon: "mdi:radio-tower", Default: true},
	{Component: "sensor", Key: "channel_util", Name: "Channel utilisation", Field: "channel_util_pct",
		Unit: "%", StateCls: "measurement", Icon: "mdi:waveform", Default: true},
	{Component: "sensor", Key: "noise", Name: "Noise floor", Field: "noise_floor_dbm", Unit: "dBm",
		Class: "signal_strength", StateCls: "measurement", Default: true},
	{Component: "sensor", Key: "rx", Name: "Received", Field: "rx", StateCls: "total_increasing",
		Icon: "mdi:download-network", Default: true},
	{Component: "sensor", Key: "tx", Name: "Transmitted", Field: "tx", StateCls: "total_increasing",
		Icon: "mdi:upload-network", Default: true},
	{Component: "sensor", Key: "nodes", Name: "Nodes heard", Field: "nodes_heard",
		StateCls: "measurement", Icon: "mdi:access-point-network", Default: true},
	{Component: "binary_sensor", Key: "radio", Name: "Radio", Field: "connected",
		Class: "connectivity", Default: true},
	{Component: "binary_sensor", Key: "duty", Name: "Duty cycle exceeded", Field: "duty_exceeded",
		Class: "problem", Default: true},

	// Present, but not recorded until someone asks for one.
	{Component: "sensor", Key: "dupes", Name: "Duplicates", Field: "rx_dupe", StateCls: "total_increasing",
		Category: "diagnostic"},
	{Component: "sensor", Key: "undecryptable", Name: "Undecryptable", Field: "rx_undecryptable",
		StateCls: "total_increasing", Category: "diagnostic"},
	{Component: "sensor", Key: "acks", Name: "ACK success", Field: "ack_pct", Unit: "%",
		StateCls: "measurement", Category: "diagnostic"},
	{Component: "sensor", Key: "queue", Name: "Transmit queue", Field: "queue", StateCls: "measurement",
		Category: "diagnostic"},
}

// A node's entities. Battery and last heard are the two people automate on, so they are the two
// that arrive switched on.
var nodeEntities = []entity{
	{Component: "sensor", Key: "battery", Name: "Battery", Field: "battery", Unit: "%",
		Class: "battery", StateCls: "measurement", Default: true},
	{Component: "sensor", Key: "last_heard", Name: "Last heard", Field: "last_heard",
		Class: "timestamp", Default: true},

	{Component: "sensor", Key: "voltage", Name: "Voltage", Field: "voltage", Unit: "V",
		Class: "voltage", StateCls: "measurement", Category: "diagnostic"},
	{Component: "sensor", Key: "snr", Name: "SNR", Field: "snr", Unit: "dB",
		StateCls: "measurement", Category: "diagnostic"},
	{Component: "sensor", Key: "rssi", Name: "RSSI", Field: "rssi", Unit: "dBm",
		Class: "signal_strength", StateCls: "measurement", Category: "diagnostic"},
	{Component: "sensor", Key: "hops", Name: "Hops away", Field: "hops", StateCls: "measurement",
		Icon: "mdi:transit-connection-variant", Category: "diagnostic"},
	{Component: "sensor", Key: "channel_util", Name: "Channel utilisation", Field: "channel_util_pct",
		Unit: "%", StateCls: "measurement", Category: "diagnostic"},
	{Component: "binary_sensor", Key: "powered", Name: "External power", Field: "powered",
		Class: "power", Category: "diagnostic"},
}

// announceSite publishes the site's discovery documents. again re-sends them even if they have
// been sent before — Home Assistant's birth message means it may have lost them — while keeping
// the record of what has been announced, because that record is what later removes a node's
// entities when it drops out of the list.
func (l *Link) announceSite(again bool) {
	l.mu.Lock()
	done := l.announced[l.site]
	l.mu.Unlock()
	if done && !again {
		return
	}

	name := "RepeaterTastic"
	model := "RepeaterTastic"
	if l.radio != nil {
		if n := l.radio.GetRelay().GetLongName(); n != "" {
			name = n
		}
		if r := l.radio.GetPresetName(); r != "" {
			model = "RepeaterTastic · " + l.radio.GetRegion() + " " + r
		}
	}
	dev := map[string]any{
		"identifiers":  []string{"rt_" + l.site},
		"name":         name,
		"manufacturer": "ScotMesh",
		"model":        model,
		"sw_version":   l.ver,
	}
	ok := true
	for _, e := range siteEntities {
		ok = l.announce(e, dev, "rt_"+l.site, l.siteState(), name) && ok
	}
	// Only remembered once the broker has it: a document dropped while reconnecting would
	// otherwise never be sent again, leaving Home Assistant with no entities at all.
	if ok {
		l.mu.Lock()
		l.announced[l.site] = true
		l.mu.Unlock()
	}
}

func (l *Link) announceNode(n *pluginv1.Node, again bool) {
	id := strings.TrimPrefix(n.GetNodeId(), "!")
	l.mu.Lock()
	done := l.announced[id]
	l.mu.Unlock()
	if done && !again {
		return
	}

	name := "Node " + id
	model := "Meshtastic node"
	if u := user(n); u != nil {
		if ln := u.GetLongName(); ln != "" {
			name = ln
		}
		if hw := u.GetHwModel().String(); hw != "" && hw != "UNSET" {
			model = hw
		}
	}
	dev := map[string]any{
		"identifiers":  []string{"rt_node_" + id},
		"name":         name,
		"manufacturer": "Meshtastic",
		"model":        model,
		"via_device":   "rt_" + l.site,
	}
	ok := true
	for _, e := range nodeEntities {
		ok = l.announce(e, dev, "rt_node_"+id, l.nodeState(id), name) && ok
	}
	if ok {
		l.mu.Lock()
		l.announced[id] = true
		l.mu.Unlock()
	}
}

// announce publishes one entity's discovery document, and reports whether the broker took it.
func (l *Link) announce(e entity, device map[string]any, uidRoot, stateTopic, devName string) bool {
	cfg := map[string]any{
		"name":                  e.Name,
		"unique_id":             uidRoot + "_" + e.Key,
		"object_id":             strings.ToLower(topicSafe(devName + "_" + e.Name)),
		"state_topic":           stateTopic,
		"value_template":        fmt.Sprintf("{{ value_json.%s }}", e.Field),
		"availability_topic":    l.availTopic(),
		"payload_available":     "online",
		"payload_not_available": "offline",
		"device":                device,
		"enabled_by_default":    e.Default,
		// A reading with no fresh publish behind it should go unavailable rather than sit there
		// looking current: a stale battery is worse than none.
		"expire_after": int(2 * l.set.staleWindow().Seconds()),
	}
	if e.Unit != "" {
		cfg["unit_of_measurement"] = e.Unit
	}
	if e.Class != "" {
		cfg["device_class"] = e.Class
	}
	if e.StateCls != "" {
		cfg["state_class"] = e.StateCls
	}
	if e.Icon != "" {
		cfg["icon"] = e.Icon
	}
	if e.Category != "" {
		cfg["entity_category"] = e.Category
	}
	if e.Component == "binary_sensor" {
		cfg["payload_on"], cfg["payload_off"] = "ON", "OFF"
		delete(cfg, "state_class")
	}
	topic := fmt.Sprintf("%s/%s/%s/%s/config", l.set.DiscoveryPrefix, e.Component, uidRoot, e.Key)
	return l.pubSure(topic, cfg)
}

// forget removes a node's entities from Home Assistant. Discovery is retained, so without this a
// node that leaves the mesh keeps its entities for ever, showing readings that stopped being true
// weeks ago.
func (l *Link) forget(id string) {
	id = strings.TrimPrefix(id, "!")
	for _, e := range nodeEntities {
		l.clear(fmt.Sprintf("%s/%s/rt_node_%s/%s/config", l.set.DiscoveryPrefix, e.Component, id, e.Key))
	}
	l.clear(l.nodeState(id))
	l.mu.Lock()
	delete(l.announced, id)
	l.mu.Unlock()
}
