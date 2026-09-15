package port

import (
	"fmt"
	"strconv"
)

// ParsePort parses a TCP/UDP port number from s.
// A valid port is an integer in the range 1..65535.
func ParsePort(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("port %q is not a number", s)
	}
	if n < 1 {
		return 0, fmt.Errorf("port %d is out of range", n)
	}
	return n, nil
}
