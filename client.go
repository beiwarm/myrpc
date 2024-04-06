package myrpc

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"myrpc/codec"
	"net"
	"sync"
	"time"
)

// 一次RPC调用需要的信息
type Call struct {
	Seq           uint64
	ServiceMethod string // format "<service>.<method>"
	Args          interface{}
	Reply         interface{}
	Error         error      //
	Done          chan *Call // 调用结束时，通过该字段通知调用方
}

func (call *Call) done() {
	call.Done <- call
}

type Client struct {
	cc       codec.Codec
	opt      *Option
	sending  sync.Mutex // 发送请求时进行保护
	header   codec.Header
	mu       sync.Mutex // 修改客户端变量时进行保护
	seq      uint64
	pending  map[uint64]*Call //存储未处理完的请求
	closing  bool             // 用户主动调用了 Close 后
	shutdown bool             // 服务器那通知连接断开
	//任意一个置为true则客户端不可用
}

var _ io.Closer = (*Client)(nil)

var ErrShutdown = errors.New("connection is shut down")

func (client *Client) Close() error {
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.closing {
		//如果客户端已经关闭了，返回一个错误
		return ErrShutdown
	}
	client.closing = true
	return client.cc.Close()
}

func (client *Client) IsAvailable() bool {
	client.mu.Lock()
	defer client.mu.Unlock()
	return !client.shutdown && !client.closing
}

func (client *Client) registerCall(call *Call) (uint64, error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.closing || client.shutdown {
		return 0, ErrShutdown
	}
	call.Seq = client.seq
	client.pending[call.Seq] = call
	client.seq++
	return call.Seq, nil
}

func (client *Client) removeCall(seq uint64) *Call {
	client.mu.Lock()
	defer client.mu.Unlock()
	call := client.pending[seq]
	delete(client.pending, seq)
	return call
}

func (client *Client) terminateCalls(err error) {
	//不允许再发送请求了
	client.sending.Lock()
	defer client.sending.Unlock()
	client.mu.Lock()
	defer client.mu.Unlock()
	client.shutdown = true
	//取消每一个还没有处理的调用
	for _, call := range client.pending {
		call.Error = err
		call.done()
	}
}

func (client *Client) receive() {
	var err error
	for err == nil {
		var h codec.Header
		if err = client.cc.ReadHeader(&h); err != nil {
			break //读不到响应，结束
		}
		//读到某个调用的响应，该请求处理完毕，移除pending
		call := client.removeCall(h.Seq)
		switch {
		case call == nil:
			//服务器处理该请求的过程中，客户端的的请求被意外移除了
			//把没用了的Body取出来丢掉
			err = client.cc.ReadBody(nil)
		case h.Error != "":
			call.Error = fmt.Errorf(h.Error)
			//服务器报了错误，返回的结果没用了，丢掉
			err = client.cc.ReadBody(nil)
			call.done()
		default:
			err = client.cc.ReadBody(call.Reply)
			if err != nil {
				call.Error = errors.New("reading body " + err.Error())
			}
			call.done()
		}

	}
	// 发生了错误，停止发送请求
	client.terminateCalls(err)
}

func NewClient(conn net.Conn, opt *Option) (*Client, error) {
	f := codec.NewCodecFuncMap[opt.CodecType]
	if f == nil {
		err := fmt.Errorf("invalid codec type %s", opt.CodecType)
		log.Println("client: codec error:", err)
		return nil, err
	}
	// 发送Option给服务器，确定编解码方式
	if err := json.NewEncoder(conn).Encode(opt); err != nil {
		log.Println("client: options error: ", err)
		_ = conn.Close()
		return nil, err
	}
	return newClientCodec(f(conn), opt), nil
}

func newClientCodec(cc codec.Codec, opt *Option) *Client {
	client := &Client{
		seq:     1,
		cc:      cc,
		opt:     opt,
		pending: make(map[uint64]*Call),
	}
	go client.receive()
	return client
}

func parseOptions(opts ...*Option) (*Option, error) {
	if len(opts) == 0 || opts[0] == nil {
		return DefaultOption, nil
	}
	if len(opts) != 1 {
		return nil, errors.New("number of options is more than 1")
	}
	opt := opts[0]
	opt.MagicNumber = DefaultOption.MagicNumber
	//没有指定CodecType的话，用DefaultCodecType
	if opt.CodecType == "" {
		opt.CodecType = DefaultOption.CodecType
	}
	return opt, nil
}

type clientResult struct {
	client *Client
	err    error
}

func Dial(network, address string, opts ...*Option) (*Client, error) {
	opt, err := parseOptions(opts...)
	if err != nil {
		return nil, err
	}

	conn, err := net.DialTimeout(network, address, opt.ConnectTimeout)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = conn.Close()
		}
	}()

	ch := make(chan clientResult)
	go func() {
		client, err := NewClient(conn, opt)
		ch <- clientResult{client: client, err: err}
	}()

	if opt.ConnectTimeout == 0 {
		result := <-ch
		return result.client, result.err
	}
	select {
	case <-time.After(opt.ConnectTimeout):
		return nil, fmt.Errorf("client: connect timeout: expect within %s", opt.ConnectTimeout)
	case result := <-ch:
		return result.client, result.err
	}
}

func (client *Client) send(call *Call) {
	client.sending.Lock()
	defer client.sending.Unlock()
	seq, err := client.registerCall(call)
	if err != nil {
		call.Error = err
		call.done()
		return
	}
	client.header.ServiceMethod = call.ServiceMethod
	client.header.Seq = seq
	client.header.Error = ""
	if err := client.cc.Write(&client.header, call.Args); err != nil {
		//发送失败就直接给这个call移除掉
		call := client.removeCall(seq)
		if call != nil {
			call.Error = err
			call.done()
		}
	}
}

// Go 创建一个Call，交给client.send发送到服务端，异步调用
func (client *Client) Go(serviceMethod string, args, reply interface{}, done chan *Call) *Call {
	if done == nil {
		done = make(chan *Call, 1)
	} else if cap(done) == 0 {
		log.Panic("client: done channel is unbuffered")
	}
	//done必须是带缓冲的，在收到服务端的响应通知调用方时，调用方可以在任意时间去使用结果
	call := &Call{
		ServiceMethod: serviceMethod,
		Args:          args,
		Reply:         reply,
		Done:          done,
	}
	client.send(call)
	return call
}

// Call 同步调用
func (client *Client) Call(serviceMethod string, args, reply interface{}, timeout time.Duration) error {
	call := client.Go(serviceMethod, args, reply, make(chan *Call, 1))
	select {
	case <-time.After(timeout):
		client.removeCall(call.Seq)
		return errors.New("client: call failed: timeout exceeded")
	case call := <-call.Done:
		return call.Error
	}
}
