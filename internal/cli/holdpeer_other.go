//go:build !darwin

package cli

import "net"

func holdPeerInfo(_ net.Conn) string { return "" }
