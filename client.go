package jsonrpc

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/41north/go-async"
	"github.com/juju/errors"
	gonanoid "github.com/matoous/go-nanoid"
	log "github.com/sirupsen/logrus"
)

var (
	idGen = func() string { return gonanoid.MustID(20) }

	ErrClosed = errors.ConstError("connection has been closed")
)

type ResponseFuture = async.Future[async.Result[Response]]

type Client interface {
	Connect() error

	Send(req Request) (Response, error)
	SendContext(ctx context.Context, req Request) (Response, error)
	SendAsync(req Request) ResponseFuture

	Close() error
}

type client struct {
	dialer   Dialer
	conn     Connection
	inFlight sync.Map
	log      *log.Entry
	closed   atomic.Bool
}

func NewClient(dialer Dialer) Client {
	return &client{
		dialer: dialer,
	}
}

func (c *client) Connect() error {
	conn, err := c.dialer.Dial()
	if err != nil {
		return err
	}

	c.conn = conn
	c.inFlight = sync.Map{}
	c.log = log.WithField("connectionId", "tbd")

	go c.readResponses()

	return nil
}

func (c *client) readResponses() {
	for !c.closed.Load() {
		// read the next response
		resp, err := c.conn.Read()
		if err != nil {
			// set the client has closed and break out of the read loop
			if err == ErrClosed {
				c.Close()
				break
			}

			// otherwise log the error
			c.log.WithError(err).Error("read failure")
		}
		future, ok := c.inFlight.LoadAndDelete(resp.Id())
		if !ok {
			c.log.
				WithField("id", resp.Id()).
				Warn("response received with unrecognised id")
		}
		future.(ResponseFuture).Set(async.NewResult[Response](resp))
	}
}

func (c *client) Close() error {
	if c.closed.CompareAndSwap(false, true) {
		// cancel any in flight requests
		c.inFlight.Range(func(key, value any) bool {
			value.(ResponseFuture).Set(async.NewResultErr[Response](ErrClosed))
			return true
		})
		return nil
	} else {
		return ErrClosed
	}
}

func (c *client) Send(req Request) (Response, error) {
	return c.SendContext(context.Background(), req)
}

func (c *client) SendContext(ctx context.Context, req Request) (Response, error) {
	future := c.SendAsync(req)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case result := <-future.Get():
		return result.Unwrap()
	}
}

func (c *client) SendAsync(req Request) ResponseFuture {
	// ensure a request id
	req.EnsureId(idGen)

	// create a future for returning the result
	future := async.NewFuture[async.Result[Response]]()

	if c.closed.Load() {
		// short circuit
		future.Set(async.NewResultErr[Response](ErrClosed))
		return future
	}

	// create an in flight entry
	c.inFlight.Store(req.Id(), future)

	// send the request
	if err := c.conn.Write(req); err != nil {
		future.Set(async.NewResultErr[Response](err))
	}

	return future
}
