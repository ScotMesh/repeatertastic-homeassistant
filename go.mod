module github.com/ScotMesh/repeatertastic-homeassistant

go 1.25.0

require (
	github.com/ScotMesh/RepeaterTastic v0.3.3
	github.com/eclipse/paho.mqtt.golang v1.5.1
	google.golang.org/protobuf v1.36.12
)

require (
	github.com/gorilla/websocket v1.5.3 // indirect
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260526163538-3dc84a4a5aaa // indirect
	google.golang.org/grpc v1.83.2 // indirect
)

// status.read and the nodes settings type land in the next RepeaterTastic release; until then
// this builds against the branch that adds them.
replace github.com/ScotMesh/RepeaterTastic => ../rt
