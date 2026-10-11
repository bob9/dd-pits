package elrs

import (
	"context"
	"fmt"
	"io"
	"net"
	"time"

	"go.bug.st/serial"
)

// NetpackPort is the TCP port the ELRS netpack listens on.
const NetpackPort = 8080

// DefaultNetpackHost is the netpack's default mDNS hostname.
const DefaultNetpackHost = "elrs-netpack.local"

// Transport is a byte pipe to a backpack device.
type Transport = io.ReadWriteCloser

// avoidedPorts are serial devices that are never backpacks (Pi UARTs,
// Windows COM1), matching the reference plugin's AVOIDED_PORTS.
var avoidedPorts = map[string]bool{
	"/dev/ttyAMA0":  true,
	"/dev/ttyAMA10": true,
	"COM1":          true,
}

const serialBaud = 460800

// DialNetpack connects to a netpack over TCP and verifies it responds to a
// version probe.
func DialNetpack(ctx context.Context, host string) (Transport, error) {
	if host == "" {
		host = DefaultNetpackHost
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, fmt.Sprint(NetpackPort)))
	if err != nil {
		return nil, fmt.Errorf("netpack %s: %w", host, err)
	}
	if _, ok := Probe(conn, 2*time.Second); !ok {
		conn.Close()
		return nil, fmt.Errorf("netpack %s: no backpack version response", host)
	}
	return conn, nil
}

// OpenSerial opens one named serial port at backpack settings (460800 8N1)
// and verifies it responds to a version probe.
func OpenSerial(portName string) (Transport, error) {
	port, err := serial.Open(portName, &serial.Mode{
		BaudRate: serialBaud,
		DataBits: 8,
		Parity:   serial.NoParity,
		StopBits: serial.OneStopBit,
	})
	if err != nil {
		return nil, fmt.Errorf("serial %s: %w", portName, err)
	}
	port.SetReadTimeout(200 * time.Millisecond)

	// Some devkits need extra time to establish the connection.
	time.Sleep(2 * time.Second)
	port.ResetInputBuffer()

	if _, ok := Probe(port, 2*time.Second); !ok {
		port.Close()
		return nil, fmt.Errorf("serial %s: no backpack version response", portName)
	}
	return port, nil
}

// ScanSerial tries every enumerated serial port (skipping known non-backpack
// ports) and returns the first that answers a version probe. Uses
// serial.GetPortsList (cgo-free on every GOOS) rather than the enumerator
// subpackage, which needs cgo on macOS and would break cross-compilation.
// ListSerialPorts returns the ports a backpack could be on, skipping the ones
// known never to be one.
//
// Exposed so an operator can PICK a port rather than type a device path they
// have no way to know — "/dev/cu.usbserial-A50285BI" is not something anyone
// should have to find out to run a race day.
func ListSerialPorts() []string {
	ports, err := serial.GetPortsList()
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(ports))
	for _, name := range ports {
		if avoidedPorts[name] {
			continue
		}
		out = append(out, name)
	}
	return out
}

func ScanSerial(ctx context.Context) (Transport, string, error) {
	ports, err := serial.GetPortsList()
	if err != nil {
		return nil, "", fmt.Errorf("enumerate serial ports: %w", err)
	}
	for _, name := range ports {
		if avoidedPorts[name] {
			continue
		}
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
		tr, err := OpenSerial(name)
		if err != nil {
			continue
		}
		return tr, name, nil
	}
	return nil, "", fmt.Errorf("no backpack found on %d serial ports", len(ports))
}

// Probe sends a version request and waits up to timeout for a matching
// response, returning the reported firmware version. This is the handshake
// both reference connection paths use to tell a backpack from other devices.
func Probe(tr Transport, timeout time.Duration) (string, bool) {
	if _, err := tr.Write(VersionRequestPacket().Encode()); err != nil {
		return "", false
	}

	deadline := time.Now().Add(timeout)
	// net.Conn reads block indefinitely without a deadline; serial ports
	// already return periodically via SetReadTimeout.
	if dl, ok := tr.(interface{ SetReadDeadline(time.Time) error }); ok {
		dl.SetReadDeadline(deadline)
		defer dl.SetReadDeadline(time.Time{})
	}

	var d Decoder
	buf := make([]byte, 256)
	for time.Now().Before(deadline) {
		n, err := tr.Read(buf)
		if n > 0 {
			for _, p := range d.Feed(buf[:n]) {
				if p.Type == TypeResponse && p.Function == FuncGetBackpackVersion {
					return decodeVersion(p.Payload), true
				}
			}
		}
		if err != nil {
			return "", false
		}
	}
	return "", false
}

func decodeVersion(payload []byte) string {
	out := make([]byte, 0, len(payload))
	for _, b := range payload {
		if b != 0 {
			out = append(out, b)
		}
	}
	return string(out)
}
