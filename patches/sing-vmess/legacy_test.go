package vmess

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/gofrs/uuid/v5"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
)

type echoHandler struct{}

func (echoHandler) NewConnectionEx(ctx context.Context, conn net.Conn, source M.Socksaddr, destination M.Socksaddr, onClose N.CloseHandlerFunc) {
	defer conn.Close()
	_, _ = io.Copy(conn, conn)
}

func (echoHandler) NewPacketConnectionEx(ctx context.Context, conn N.PacketConn, source M.Socksaddr, destination M.Socksaddr, onClose N.CloseHandlerFunc) {
	conn.Close()
}

const testUUID = "b831381d-6324-4d53-ad4f-8cda48b30811"

func newTestService(t *testing.T, alterId int, opts ...ServiceOption) *Service[int] {
	t.Helper()
	service := NewService[int](echoHandler{}, opts...)
	if err := service.UpdateUsers([]int{1}, []string{testUUID}, []int{alterId}); err != nil {
		t.Fatal(err)
	}
	if err := service.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { service.Close() })
	return service
}

func roundTrip(service *Service[int], client *Client) error {
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	_ = clientConn.SetDeadline(time.Now().Add(2 * time.Second))
	errCh := make(chan error, 1)
	go func() {
		errCh <- service.NewConnection(context.Background(), serverConn, M.Socksaddr{}, nil)
	}()
	conn, err := client.DialConn(clientConn, M.ParseSocksaddr("example.com:80"))
	if err != nil {
		return err
	}
	msg := []byte("hello vmess")
	if _, err := conn.Write(msg); err != nil {
		select {
		case herr := <-errCh:
			if herr != nil {
				return herr
			}
		default:
		}
		return err
	}
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, got); err != nil {
		select {
		case herr := <-errCh:
			if herr != nil {
				return herr
			}
		default:
		}
		return err
	}
	if string(got) != string(msg) {
		return io.ErrUnexpectedEOF
	}
	return nil
}

// legacyPrimaryClient builds a legacy client that authenticates with the primary
// UUID (alterId=0 in the subscription), as Shadowrocket and older V2Ray clients do.
func legacyPrimaryClient(t *testing.T, offset time.Duration) *Client {
	t.Helper()
	c, err := NewClient(testUUID, "aes-128-gcm", 1, ClientWithTimeFunc(func() time.Time { return time.Now().Add(offset) }))
	if err != nil {
		t.Fatal(err)
	}
	c.alterKey = uuid.Must(uuid.FromString(testUUID))
	return c
}

func TestServiceAcceptsAEAD(t *testing.T) {
	for _, opts := range [][]ServiceOption{nil, {ServiceWithDisableHeaderProtection()}} {
		service := newTestService(t, 0, opts...)
		c, err := NewClient(testUUID, "aes-128-gcm", 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := roundTrip(service, c); err != nil {
			t.Fatalf("AEAD rejected: %v", err)
		}
	}
}

func TestServiceAcceptsLegacyPrimaryUUIDWithAlterIdZero(t *testing.T) {
	for _, opts := range [][]ServiceOption{nil, {ServiceWithDisableHeaderProtection()}} {
		service := newTestService(t, 0, opts...)
		for _, off := range []time.Duration{0, -30 * time.Second, 30 * time.Second} {
			if err := roundTrip(service, legacyPrimaryClient(t, off)); err != nil {
				t.Fatalf("legacy primary UUID offset %v rejected: %v", off, err)
			}
		}
	}
}

func TestServiceAcceptsLegacyAlterId(t *testing.T) {
	service := newTestService(t, 1)
	c, err := NewClient(testUUID, "aes-128-gcm", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := roundTrip(service, c); err != nil {
		t.Fatalf("legacy AlterID rejected: %v", err)
	}
}

func TestServiceRejectsLegacyOutsideWindow(t *testing.T) {
	service := newTestService(t, 0)
	if err := roundTrip(service, legacyPrimaryClient(t, 5*time.Minute)); err == nil {
		t.Fatal("legacy request outside window accepted")
	}
}

func TestServiceRejectsUnknownUser(t *testing.T) {
	service := newTestService(t, 0)
	c, err := NewClient("75eae340-a037-4cc2-b64c-ae5dc26cc897", "aes-128-gcm", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := roundTrip(service, c); err == nil {
		t.Fatal("unknown user accepted")
	}
}
