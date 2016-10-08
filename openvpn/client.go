package openvpn

import (
	"bytes"
	"fmt"
	"io"
	"net"

	"github.com/apparentlymart/go-openvpn-mgmt/demux"
)

var newline = []byte{'\n'}
var successPrefix = []byte("SUCCESS: ")
var errorPrefix = []byte("ERROR: ")
var endMessage = []byte("END")

type MgmtClient struct {
	wr      io.Writer
	replies <-chan []byte
}

// NewClient creates a new MgmtClient that communicates via the given
// io.ReadWriter and emits events on the given channel.
//
// eventCh should be a buffered channel with a sufficient buffer depth
// such that it cannot be filled under the expected event volume. Event
// volume depends on which events are enabled and how they are configured;
// some of the event-enabling functions have further discussion how frequently
// events are likely to be emitted, but the caller should also factor in
// how long its own event *processing* will take, since slow event
// processing will create back-pressure that could cause this buffer to
// fill faster.
//
// It probably goes without saying given the previous paragraph, but the
// caller *must* constantly read events from eventCh to avoid its buffer
// becoming full. Events and replies are received on the same channel
// from OpenVPN, so if writing to eventCh blocks then this will also block
// responses from the client's various command methods.
//
// eventCh will be closed to signal the closing of the client connection,
// whether due to graceful shutdown or to an error. In the case of error,
// a FatalEvent will be emitted on the channel as the last event before it
// is closed. Connection errors may also concurrently surface as error
// responses from the client's various command methods, should an error
// occur while we await a reply.
func NewClient(conn io.ReadWriter, eventCh chan<- Event) *MgmtClient {
	replyCh := make(chan []byte)
	rawEventCh := make(chan []byte) // not buffered because eventCh should be

	go demux.Demultiplex(conn, replyCh, rawEventCh)

	// Get raw events and upgrade them into proper event types before
	// passing them on to the caller's event channel.
	go func() {
		for raw := range rawEventCh {
			eventCh <- upgradeEvent(raw)
		}
		close(eventCh)
	}()

	return &MgmtClient{
		// replyCh acts as the reader for our ReadWriter, so we only
		// need to retain the io.Writer for it, so we can send commands.
		wr:      conn,
		replies: replyCh,
	}
}

// Dial is a convenience wrapper around NewClient that handles the common
// case of opening an TCP/IP socket to an OpenVPN management port and creating
// a client for it.
//
// See the NewClient docs for discussion about the requirements for eventCh.
//
// OpenVPN will create a suitable management port if launched with the
// following command line option:
//
//    --management <ipaddr> <port>
//
// Address may an IPv4 address, an IPv6 address, or a hostname that resolves
// to either of these, followed by a colon and then a port number.
func Dial(addr string, eventCh chan<- Event) (*MgmtClient, error) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}

	return NewClient(conn, eventCh), nil
}

// HoldRelease instructs OpenVPN to release any management hold preventing
// it from proceeding, but to retain the state of the hold flag such that
// the daemon will hold again if it needs to reconnect for any reason.
//
// OpenVPN can be instructed to activate a management hold on startup by
// running it with the following option:
//
//     --management-hold
//
// Instructing OpenVPN to hold gives your client a chance to connect and
// do any necessary configuration before a connection proceeds, thus avoiding
// the problem of missed events.
//
// When OpenVPN begins holding, or when a new management client connects while
// a hold is already in effect, a HoldEvent will be emitted on the event
// channel.
func (c *MgmtClient) HoldRelease() error {
	_, err := c.simpleCommand("hold release")
	return err
}

// SetStateEvents either enables or disables asynchronous events for changes
// in the OpenVPN connection state.
//
// When enabled, a StateEvent will be emitted from the event channel each
// time the connection state changes. See StateEvent for more information
// on the event structure.
func (c *MgmtClient) SetStateEvents(on bool) error {
	var err error
	if on {
		_, err = c.simpleCommand("state on")
	} else {
		_, err = c.simpleCommand("state off")
	}
	return err
}

// SetEchoEvents either enables or disables asynchronous events for "echo"
// commands sent from a remote server to our managed OpenVPN client.
//
// When enabled, an EchoEvent will be emitted from the event channel each
// time the server sends an echo command. See EchoEvent for more information.
func (c *MgmtClient) SetEchoEvents(on bool) error {
	var err error
	if on {
		_, err = c.simpleCommand("echo on")
	} else {
		_, err = c.simpleCommand("echo off")
	}
	return err
}

func (c *MgmtClient) sendCommand(cmd []byte) error {
	_, err := c.wr.Write(cmd)
	if err != nil {
		return err
	}
	_, err = c.wr.Write(newline)
	return err
}

// sendCommandPayload can be called after sendCommand for
// commands that expect a multi-line input payload.
//
// The buffer given in 'payload' *must* end with a newline,
// or else the protocol will be broken.
func (c *MgmtClient) sendCommandPayload(payload []byte) error {
	_, err := c.wr.Write(payload)
	if err != nil {
		return err
	}
	_, err = c.wr.Write(endMessage)
	if err != nil {
		return err
	}
	_, err = c.wr.Write(newline)
	return err
}

func (c *MgmtClient) readCommandResult() ([]byte, error) {
	reply, ok := <-c.replies
	if !ok {
		return nil, fmt.Errorf("connection closed while awaiting result")
	}

	if bytes.HasPrefix(reply, successPrefix) {
		result := reply[len(successPrefix):]
		return result, nil
	}

	if bytes.HasPrefix(reply, errorPrefix) {
		message := reply[len(errorPrefix):]
		return nil, ErrorFromServer(message)
	}

	return nil, fmt.Errorf("malformed result message")
}

func (c *MgmtClient) readCommandResponsePayload() ([][]byte, error) {
	lines := make([][]byte, 0, 10)

	for {
		line, ok := <-c.replies
		if !ok {
			// We'll give the caller whatever we got before the connection
			// closed, in case it's useful for debugging.
			return lines, fmt.Errorf("connection closed before END recieved")
		}

		if bytes.Equal(line, endMessage) {
			break
		}

		lines = append(lines, line)
	}

	return lines, nil
}

func (c *MgmtClient) simpleCommand(cmd string) ([]byte, error) {
	err := c.sendCommand([]byte(cmd))
	if err != nil {
		return nil, err
	}
	return c.readCommandResult()
}
