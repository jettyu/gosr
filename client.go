package gosr

import (
	"errors"
	"io"
	"log"
	"sync"
	"time"
)

// Call represents an active RPC.
type Call struct {
	Args  interface{} // The argument to the function (*struct).
	Reply interface{} // The reply from the function (*struct).
	Error error       // After completion, the error status.
	Done  chan *Call  // Strobes when call is complete.
}

// Request ...
type Request interface {
	GetSeq() interface{}
}

// Response ...
type Response interface {
	GetSeq() interface{}
	Err() error
}

// A ClientCodec implements writing of RPC requests and
// reading of RPC responses for the p side of an RPC session.
// The p calls WriteRequest to write a request to the connection
// and calls ReadResponseHeader and ReadResponseBody in pairs
// to read responses. The p calls Close when finished with the
// connection. ReadResponseBody may be called with a nil
// argument to force the body of the response to be read and then
// discarded.
// See NewClient's comment for information about concurrent access.
type ClientCodec interface {
	BuildRequest(args interface{}) (Request, error)
	WriteRequest(req Request, args interface{}) error
	ReadResponseHeader() (Response, error)
	ReadResponseBody(resp Response, reply interface{}) error
	Close() error
}

// Client represents an RPC Client.
// There may be multiple outstanding Calls associated
// with a single Client, and a Client may be used by
// multiple goroutines simultaneously.
type Client struct {
	codec    ClientCodec
	mutex    sync.Mutex // protects following
	pending  map[interface{}]*Call
	closing  bool // user has called Close
	shutdown bool // server has told us to stop
	timeout  time.Duration
	waited   sync.WaitGroup
}

// Wait ...
func (p *Client) Wait() {
	p.waited.Wait()
}

// NewClientWithCodec is like NewClient but uses the specified
// codec to encode requests and decode responses.
func NewClientWithCodec(codec ClientCodec, timeout ...time.Duration) *Client {
	p := &Client{
		codec:   codec,
		pending: make(map[interface{}]*Call),
	}
	if len(timeout) > 0 {
		p.timeout = timeout[0]
	}
	p.waited.Add(1)
	go p.input()
	return p
}

// Close calls the underlying codec's Close method. If the connection is already
// shutting down, ErrClosed is returned.
func (p *Client) Close() error {
	p.mutex.Lock()
	if p.closing {
		p.mutex.Unlock()
		return ErrClosed
	}
	p.closing = true
	p.mutex.Unlock()
	return p.codec.Close()
}

// Go invokes the function asynchronously. It returns the Call structure representing
// the invocation. The done channel will signal when the call is complete by returning
// the same Call object. If done is nil, Go will allocate a new channel.
// If non-nil, done must be buffered or Go will deliberately crash.
func (p *Client) Go(args interface{}, reply interface{}, done chan *Call) *Call {
	call := new(Call)
	call.Args = args
	call.Reply = reply
	if done == nil {
		done = make(chan *Call, 10) // buffered.
	} else {
		// If caller passes done != nil, it must arrange that
		// done has enough buffer for the number of simultaneous
		// RPCs that will be using that channel. If the channel
		// is totally unbuffered, it's best not to run at all.
		if cap(done) == 0 {
			log.Panic("rpc: done channel is unbuffered")
		}
	}
	call.Done = done
	p.send(call)
	return call
}

// Call invokes the named function, waits for it to complete, and returns its error status.
func (p *Client) Call(args interface{}, reply interface{}) error {
	call := <-p.Go(args, reply, make(chan *Call, 1)).Done
	return call.Error
}

func (p *Client) send(call *Call) {
	req, err := p.codec.BuildRequest(call.Args)
	if err != nil {
		call.Error = err
		call.done()
		return
	}
	// Register this call.
	p.mutex.Lock()
	if p.shutdown || p.closing {
		p.mutex.Unlock()
		call.Error = ErrClosed
		call.done()
		return
	}
	seq := req.GetSeq()
	_, ok := p.pending[seq]
	if ok {
		p.mutex.Unlock()
		call.Error = Errof("[chanrpc] repeated seq, seq=%v", seq)
		call.done()
		return
	}
	p.pending[seq] = call
	p.mutex.Unlock()
	if p.timeout > 0 {
		time.AfterFunc(p.timeout, func() {
			p.mutex.Lock()
			timeoutCall, ok := p.pending[seq]
			if ok && call == timeoutCall {
				delete(p.pending, seq)
				call = timeoutCall
			} else {
				call = nil
			}
			p.mutex.Unlock()
			if call != nil {
				call.Error = ErrTimeOut
				call.done()
			}
		})
	}
	// Encode and send the request.
	err = p.codec.WriteRequest(req, call.Args)
	if err != nil {
		p.mutex.Lock()
		call = p.pending[seq]
		delete(p.pending, seq)
		p.mutex.Unlock()
		if call != nil {
			call.Error = err
			call.done()
		}
	}
}

func (p *Client) input() {
	defer p.waited.Done()
	var err error
	var response Response
	for err == nil {
		response, err = p.codec.ReadResponseHeader()
		if err != nil {
			break
		}
		seq := response.GetSeq()
		p.mutex.Lock()
		call := p.pending[seq]
		delete(p.pending, seq)
		p.mutex.Unlock()

		switch {
		case call == nil:
			// We've got no pending call. That usually means that
			// WriteRequest partially failed, and call was already
			// removed; response is a server telling us about an
			// error reading request body. We should still attempt
			// to read error body, but there's no one to give it to.
			err = p.codec.ReadResponseBody(response, nil)
			if err != nil {
				err = errors.New("reading error body: " + err.Error())
			}
		case response.Err() != nil:
			// We've got an error response. Give this to the request;
			// any subsequent requests will get the ReadResponseBody
			// error if there is one.
			call.Error = response.Err()
			err = p.codec.ReadResponseBody(response, nil)
			if err != nil {
				err = errors.New("reading error body: " + err.Error())
			}
			call.done()
		default:
			err = p.codec.ReadResponseBody(response, call.Reply)
			if err != nil {
				call.Error = errors.New("reading body " + err.Error())
			}
			call.done()
		}
	}
	// Terminate pending calls.
	p.mutex.Lock()
	p.shutdown = true
	closing := p.closing
	if err == io.EOF {
		if closing {
			err = ErrClosed
		} else {
			err = io.ErrUnexpectedEOF
		}
	}
	for _, call := range p.pending {
		call.Error = err
		call.done()
	}
	p.mutex.Unlock()
	if debugLog && err != io.EOF && !closing {
		log.Println("rpc: p protocol error:", err)
	}
}

// If set, print log statements for internal and I/O errors.
var debugLog = false

func (call *Call) done() {
	select {
	case call.Done <- call:
		// ok
	default:
		// We don't want to block here. It is the caller's responsibility to make
		// sure the channel has enough buffer space. See comment in Go().
		if debugLog {
			log.Println("rpc: discarding Call reply due to insufficient Done chan capacity")
		}
	}
}
