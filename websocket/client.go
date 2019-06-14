package websocket

import (
	"encoding/json"
	"io/ioutil"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	ws "github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"
	"github.com/stripe/stripe-cli/useragent"
)

//
// Public constants
//

//
// Public types
//

// Config contains the optional configuration parameters of a Client.
type Config struct {
	ConnectAttemptWait time.Duration

	Dialer *ws.Dialer

	Log *log.Logger

	// Force use of unencrypted ws:// protocol instead of wss://
	NoWSS bool

	PingPeriod time.Duration

	PongWait time.Duration

	// Interval at which the websocket client should reset the connection
	ReconnectInterval time.Duration

	UnixSocket string

	WriteWait time.Duration

	WebhookEventHandler WebhookEventHandler
}

// WebhookEventHandler handles a webhook event.
type WebhookEventHandler interface {
	ProcessWebhookEvent(*WebhookEvent)
}

// WebhookEventHandlerFunc is an adapter to allow the use of ordinary
// functions as webhook event handlers. If f is a function with the
// appropriate signature, WebhookEventHandlerFunc(f) is a
// WebhookEventHandler that calls f.
type WebhookEventHandlerFunc func(*WebhookEvent)

// ProcessWebhookEvent calls f(msg).
func (f WebhookEventHandlerFunc) ProcessWebhookEvent(msg *WebhookEvent) {
	f(msg)
}

// Client is the client used to receive webhook requests from Stripe
// and send back webhook responses from the local endpoint to Stripe.
type Client struct {
	// URL the client connects to
	URL string

	// ID sent by the client in the `Websocket-Id` header when connecting
	WebSocketID string

	// Optional configuration parameters
	cfg *Config

	conn          *ws.Conn
	done          chan struct{}
	isConnected   bool
	notifyClose   chan error
	send          chan *OutgoingMessage
	stopReadPump  chan struct{}
	stopWritePump chan struct{}
	wg            *sync.WaitGroup
}

// Run starts listening for incoming webhook requests from Stripe.
func (c *Client) Run() {
	for {
		c.isConnected = false
		c.cfg.Log.WithFields(log.Fields{
			"prefix": "websocket.client.Run",
		}).Debug("Attempting to connect to Stripe")

		for !c.connect() {
			c.cfg.Log.WithFields(log.Fields{
				"prefix": "websocket.client.Run",
			}).Debug("Failed to connect to Stripe. Retrying...")
			time.Sleep(c.cfg.ConnectAttemptWait)
		}
		select {
		case <-c.done:
			close(c.send)
			close(c.stopReadPump)
			close(c.stopWritePump)
			return
		case <-c.notifyClose:
			c.cfg.Log.WithFields(log.Fields{
				"prefix": "websocket.client.Run",
			}).Debug("Disconnected from Stripe")
			close(c.stopReadPump)
			close(c.stopWritePump)
			c.wg.Wait()
		case <-time.After(c.cfg.ReconnectInterval):
			c.cfg.Log.WithFields(log.Fields{
				"prefix": "websocket.Client.Run",
			}).Debug("Resetting the connection")
			close(c.stopReadPump)
			close(c.stopWritePump)
			if c.conn != nil {
				c.conn.Close() // #nosec G104
			}
			c.wg.Wait()
		}
	}
}

// Stop stops listening for incoming webhook events.
func (c *Client) Stop() {
	close(c.done)
}

// SendMessage sends a message to Stripe through the websocket.
func (c *Client) SendMessage(msg *OutgoingMessage) {
	c.send <- msg
}

// connect makes a single attempt to connect to the websocket URL. It returns
// the success of the attempt.
func (c *Client) connect() bool {
	header := http.Header{}
	// Disable compression by requiring "identity"
	header.Set("Accept-Encoding", "identity")
	header.Set("User-Agent", useragent.GetEncodedUserAgent())
	header.Set("X-Stripe-Client-User-Agent", useragent.GetEncodedStripeUserAgent())
	header.Set("Websocket-Id", c.WebSocketID)

	url := c.URL
	if c.cfg.NoWSS && strings.HasPrefix(url, "wss") {
		url = "ws" + strings.TrimPrefix(c.URL, "wss")
	}
	c.cfg.Log.WithFields(log.Fields{
		"prefix": "websocket.Client.connect",
		"url":    url,
	}).Debug("Dialing websocket")

	conn, _, err := c.cfg.Dialer.Dial(url, header)
	if err != nil {
		c.cfg.Log.WithFields(log.Fields{
			"prefix": "websocket.Client.connect",
			"error":  err,
		}).Debug("Websocket connection error")
		return false
	}
	c.changeConnection(conn)
	c.isConnected = true

	c.wg = &sync.WaitGroup{}
	c.wg.Add(2)
	go c.readPump()
	go c.writePump()

	c.cfg.Log.WithFields(log.Fields{
		"prefix": "websocket.client.connect",
	}).Debug("Connected!")
	return true
}

// changeConnection takes a new connection and recreates the channels.
func (c *Client) changeConnection(conn *ws.Conn) {
	c.conn = conn
	c.notifyClose = make(chan error)
	c.stopReadPump = make(chan struct{})
	c.stopWritePump = make(chan struct{})
}

// readPump pumps messages from the websocket connection and pushes them into
// RequestHandler's ProcessWebhookRequest.
//
// The application runs readPump in a per-connection goroutine. The application
// ensures that there is at most one reader on a connection by executing all
// reads from this goroutine.
func (c *Client) readPump() {
	defer c.wg.Done()

	err := c.conn.SetReadDeadline(time.Now().Add(c.cfg.PongWait))
	if err != nil {
		c.cfg.Log.Warn("SetReadDeadline error: ", err)
	}
	c.conn.SetPongHandler(func(string) error {
		c.cfg.Log.WithFields(log.Fields{
			"prefix": "websocket.Client.readPump",
		}).Debug("Received pong message")
		err := c.conn.SetReadDeadline(time.Now().Add(c.cfg.PongWait))
		if err != nil {
			c.cfg.Log.Warn("SetReadDeadline error: ", err)
		}
		return nil
	})

	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			select {
			case <-c.stopReadPump:
				c.cfg.Log.WithFields(log.Fields{
					"prefix": "websocket.Client.readPump",
				}).Debug("stopReadPump")
			default:
				if !ws.IsCloseError(err) {
					c.cfg.Log.Error("read error: ", err)
				} else if ws.IsUnexpectedCloseError(err, ws.CloseNormalClosure) {
					c.cfg.Log.Error("read error: ", err)
				}
				c.notifyClose <- err
			}
			return
		}

		c.cfg.Log.WithFields(log.Fields{
			"prefix":  "websocket.Client.readPump",
			"message": string(data),
		}).Debug("Incoming message")

		var msg IncomingMessage
		if err = json.Unmarshal(data, &msg); err != nil {
			c.cfg.Log.Warn("Received malformed message: ", err)
			continue
		}

		if msg.WebhookEvent != nil {
			go c.cfg.WebhookEventHandler.ProcessWebhookEvent(msg.WebhookEvent)
		}
	}
}

// writePump pumps messages to the websocket connection that are queued with
// SendWebhookResponse.
//
// A goroutine running writePump is started for each connection. The
// application ensures that there is at most one writer to a connection by
// executing all writes from this goroutine.
func (c *Client) writePump() {
	ticker := time.NewTicker(c.cfg.PingPeriod)
	defer func() {
		ticker.Stop()
		c.wg.Done()
	}()

	for {
		select {
		case whResp, ok := <-c.send:
			err := c.conn.SetWriteDeadline(time.Now().Add(c.cfg.WriteWait))
			if err != nil {
				c.cfg.Log.Warn("SetWriteDeadline error: ", err)
			}
			if !ok {
				c.cfg.Log.WithFields(log.Fields{
					"prefix": "websocket.Client.writePump",
				}).Debug("Sending close message")
				err = c.conn.WriteMessage(ws.CloseMessage, ws.FormatCloseMessage(ws.CloseNormalClosure, ""))
				if err != nil {
					c.cfg.Log.Warn("WriteMessage error: ", err)
				}
				return
			}

			c.cfg.Log.WithFields(log.Fields{
				"prefix": "websocket.Client.writePump",
			}).Debug("Sending text message")

			err = c.conn.WriteJSON(whResp)
			if err != nil {
				if ws.IsUnexpectedCloseError(err, ws.CloseNormalClosure) {
					c.cfg.Log.Error("write error: ", err)
				}
				// Requeue the message to be processed when writePump restarts
				c.send <- whResp
				c.notifyClose <- err
				return
			}
		case <-ticker.C:
			err := c.conn.SetWriteDeadline(time.Now().Add(c.cfg.WriteWait))
			if err != nil {
				c.cfg.Log.Warn("SetWriteDeadline error: ", err)
			}
			c.cfg.Log.WithFields(log.Fields{
				"prefix": "websocket.Client.writePump",
			}).Debug("Sending ping message")
			if err = c.conn.WriteMessage(ws.PingMessage, nil); err != nil {
				if ws.IsUnexpectedCloseError(err, ws.CloseNormalClosure) {
					c.cfg.Log.Error("write error: ", err)
				}
				c.notifyClose <- err
				return
			}
		case <-c.stopWritePump:
			c.cfg.Log.WithFields(log.Fields{
				"prefix": "websocket.Client.writePump",
			}).Debug("stopWritePump")
			return
		}
	}
}

//
// Public functions
//

// NewClient returns a new Client.
func NewClient(url string, webSocketID string, cfg *Config) *Client {
	if cfg == nil {
		cfg = &Config{}
	}
	if cfg.ConnectAttemptWait == 0 {
		cfg.ConnectAttemptWait = defaultConnectAttemptWait
	}
	if cfg.Dialer == nil {
		cfg.Dialer = newWebSocketDialer(cfg.UnixSocket)
	}
	if cfg.Log == nil {
		cfg.Log = &log.Logger{Out: ioutil.Discard}
	}
	if cfg.PongWait == 0 {
		cfg.PongWait = defaultPongWait
	}
	if cfg.PingPeriod == 0 {
		cfg.PingPeriod = (cfg.PongWait * 9) / 10
	}
	if cfg.ReconnectInterval == 0 {
		cfg.ReconnectInterval = defaultReconnectInterval
	}
	if cfg.WriteWait == 0 {
		cfg.WriteWait = defaultWriteWait
	}
	if cfg.WebhookEventHandler == nil {
		cfg.WebhookEventHandler = nullWebhookEventHandler
	}

	return &Client{
		URL:         url,
		WebSocketID: webSocketID,
		cfg:         cfg,
		done:        make(chan struct{}),
		send:        make(chan *OutgoingMessage),
	}
}

//
// Private constants
//

const (
	defaultConnectAttemptWait = 10 * time.Second

	defaultPongWait = 10 * time.Second

	defaultReconnectInterval = 60 * time.Second

	defaultWriteWait = 10 * time.Second
)

//
// Private variables
//

var subprotocols = [...]string{"stripecli-devproxy-v1"}

var nullWebhookEventHandler = WebhookEventHandlerFunc(func(*WebhookEvent) {})

//
// Private functions
//

func newWebSocketDialer(unixSocket string) *ws.Dialer {
	var dialer *ws.Dialer
	if unixSocket != "" {
		dialFunc := func(network, addr string) (net.Conn, error) {
			return net.Dial("unix", unixSocket)
		}
		dialer = &ws.Dialer{
			HandshakeTimeout: 10 * time.Second,
			NetDial:          dialFunc,
			Subprotocols:     subprotocols[:],
		}
	} else {
		dialer = &ws.Dialer{
			HandshakeTimeout: 10 * time.Second,
			Proxy:            http.ProxyFromEnvironment,
			Subprotocols:     subprotocols[:],
		}
	}
	return dialer
}