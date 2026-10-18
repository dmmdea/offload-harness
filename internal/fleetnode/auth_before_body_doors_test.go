package fleetnode

// The bearer is checked before a request body is read at every door that takes one, not only the three that share the
// admission path (auth_before_body_test.go). The chat lane, both kvslot routes, the PAIR relay, the compose-project door
// and the four queue routes that carry a body each answer a caller without the credential before they have buffered a
// byte of it, so what such a caller costs the node is the auth verdict and nothing else. The table is refusalDoors (every
// door that refuses a credential, in refusal_every_door_test.go), and route_table_test.go holds it complete: a route the
// node serves that takes a body has a row there, so it is held to this rule too.

import (
	"net/http"
	"testing"
)

// bodyDoors are the doors of refusalDoors that take a request body.
func bodyDoors() []refusalDoor {
	var out []refusalDoor
	for _, d := range refusalDoors {
		if takesBody(d.method) {
			out = append(out, d)
		}
	}
	return out
}

func TestABearerlessPostToEveryDoorReadsNoBodyBytes(t *testing.T) {
	const size = 20 << 20
	for _, door := range bodyDoors() {
		for name, hdr := range map[string]map[string]string{
			"no bearer":    nil,
			"wrong bearer": {"Authorization": "Bearer " + wrongKey},
		} {
			t.Run(door.name+" with "+name, func(t *testing.T) {
				s := refusalServer(t, holderKey)
				cb := &countingBody{left: size}
				rec := send(s, door.method, door.path, cb, size, "application/json", hdr)
				if cb.read != 0 {
					t.Errorf("the node read %d byte(s) of a body it was going to refuse for its credential", cb.read)
				}
				if rec.Code != http.StatusUnauthorized {
					t.Errorf("status = %d, want 401 (body %s)", rec.Code, rec.Body.String())
				}
			})
		}
	}
}

// A node with no token that listens beyond loopback refuses every door that is open there, and reads no body to do it.
func TestAPostToEveryDoorOfATokenlessNodeBeyondLoopbackReadsNoBodyBytes(t *testing.T) {
	const size = 20 << 20
	for _, door := range bodyDoors() {
		if door.noKeyStatus == 0 {
			continue
		}
		t.Run(door.name, func(t *testing.T) {
			s := refusalServer(t, "")
			cb := &countingBody{left: size}
			rec := send(s, door.method, door.path, cb, size, "application/json", nil)
			if cb.read != 0 {
				t.Errorf("the node read %d byte(s) of a body it was going to refuse for want of a token", cb.read)
			}
			if rec.Code != door.noKeyStatus {
				t.Errorf("status = %d, want %d (body %s)", rec.Code, door.noKeyStatus, rec.Body.String())
			}
		})
	}
}
