# RepeaterTastic for Home Assistant

A **community-built** [RepeaterTastic](https://github.com/ScotMesh/RepeaterTastic) plugin that
publishes your site and the nodes you pick to Home Assistant over MQTT. It is not affiliated with,
endorsed by, or supported by Home Assistant or the Open Home Foundation. They arrive as devices with sensors; there is
nothing to configure at the Home Assistant end.

```
Plugins → Browse store → Home Assistant → Install
```

Needs RepeaterTastic 0.4.0 or newer, which is where `status.read` and the node
picker arrived.

Then fill in your broker and tick the nodes you care about.

## What turns up in Home Assistant

**The site**, as one device: airtime, channel utilisation, noise floor, packets received and
transmitted, nodes heard, whether the radio is up, and whether you are over your duty cycle.
Duplicates, undecryptable packets, ACK success and the transmit queue are there too, switched off
until you ask for one.

**Each node you pick**, as its own device hanging off the site: battery, when it was last heard,
and — again switched off by default — voltage, SNR, RSSI, hops away and channel utilisation. A node
on external power reports `powered` rather than a 101% battery, because that is what Meshtastic's
101 means.

Optionally, **messages**: every text heard on a channel, or every direct message to your
identities, published as an MQTT event you can trigger automations on. Both are off by default —
turning the second one on means anyone with your broker can read your DMs.

## Settings worth explaining

| Setting | What it does |
| --- | --- |
| Nodes to publish | The ones you tick are always published and never dropped for going quiet. |
| Also publish | `picked` is just those. `telemetry` adds any node reporting a battery, `direct` any node heard without a hop, `all` everything heard recently. |
| At most | A ceiling on those extras, so a busy mesh doesn't fill Home Assistant. Picked nodes don't count against it. |
| Forget a node after | Hours of silence before an unpicked node's entities are removed. Picks are exempt. |
| Publish every | Seconds between updates. Nothing goes on the mesh — this only costs your broker. |

## How it stays cheap

Every entity of a device reads from that device's one state topic through a `value_template`, so a
single publish refreshes all of them. The detail sensors are announced with
`enabled_by_default: false`, so they exist without being recorded until someone asks for one. And
the plugin reads the radios' figures from RepeaterTastic rather than listening to every packet, so
a busy mesh doesn't turn into a busy broker.

Discovery documents are retained, so Home Assistant finds everything whenever it starts, and the
plugin re-announces when Home Assistant publishes its birth message — a broker that lost its
retained store would otherwise leave you with no entities at all. When a node drops out of the
list, its entities are removed rather than left showing readings that stopped being true weeks ago.

## Permissions

| Permission | Why |
| --- | --- |
| `status.read` | The site's figures: airtime, noise floor, channel use, the counters. |
| `nodes.read` | The node database, for batteries and last-heard. |
| `messages.read` | Only used if you turn message publishing on. Untick it otherwise. |

It sends nothing on the radio, so it asks for no transmit permission and uses none of your send
budget.

## Building it yourself

```
make test      # vet and the tests
make bundle    # dist/repeatertastic-homeassistant-<version>.zip, for Plugins → Install plugin
```

The bundle carries builds for 64-bit and 32-bit Raspberry Pi OS and x86-64.

## Licence

GPL-3.0-or-later. See [LICENSE](LICENSE).

The Home Assistant name and logo are trademarks of the Open Home Foundation. The logo is used here
only to identify what this plugin talks to, and the icon comes from Home Assistant's own
[brands](https://brands.home-assistant.io) service.
