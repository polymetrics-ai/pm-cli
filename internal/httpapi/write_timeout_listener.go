package httpapi

import (
	"errors"
	"net"
)

// CloseOnWriteTimeout decorates a raw listener so an accepted connection closes
// its underlying transport before returning a write timeout. Install it beneath
// any TLS listener so TLS identity remains visible to net/http.
func CloseOnWriteTimeout(listener net.Listener) net.Listener {
	if listener == nil {
		return nil
	}
	return closeOnWriteTimeoutListener{Listener: listener}
}

type closeOnWriteTimeoutListener struct {
	net.Listener
}

func (listener closeOnWriteTimeoutListener) Accept() (net.Conn, error) {
	connection, err := listener.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return closeOnWriteTimeoutConnection{Conn: connection}, nil
}

type closeOnWriteTimeoutConnection struct {
	net.Conn
}

func (connection closeOnWriteTimeoutConnection) Write(payload []byte) (int, error) {
	written, err := connection.Conn.Write(payload)
	if err != nil {
		var networkError net.Error
		if errors.As(err, &networkError) && networkError.Timeout() {
			_ = connection.Conn.Close()
		}
	}
	return written, err
}
