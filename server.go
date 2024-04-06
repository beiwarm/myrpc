package myrpc

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"myrpc/codec"
	"net"
	"reflect"
	"strings"
	"sync"
	"time"
)

const MagicNumber = 0x3bef5c

// Option 通常协议协商的这部分信息是需要设计固定的字节来传输的，1字节表示序列化方式，2字节表示压缩方式，3-6字节表示Header长度，7-10表示Body长度，
// 为了简单实现这里使用Json编码来传输协议头，协议头用于客户端和服务端协商编解码方式，
// 在一次连接中，Option固定在报文的最开始，Header和Body可以有多个
type Option struct {
	MagicNumber    int32         // MagicNumber，在数据流的最开始出现，一个固定长度的字段，用于判断使用的通信协议是否相同
	CodecType      codec.Type    // 客户端向服务端指定编解码方式
	ConnectTimeout time.Duration //客户端连接超时时间
	HandleTimeout  time.Duration //服务器处理超时时间
}

var DefaultOption = &Option{
	MagicNumber:    MagicNumber,
	CodecType:      codec.GobType,
	ConnectTimeout: time.Second * 5,
}

type Server struct {
	serviceMap sync.Map
}

func NewServer() *Server {
	return &Server{}
}

func (server *Server) Accept(lis net.Listener) {
	for {
		conn, err := lis.Accept() //阻塞等待有客户端连接进来，然后开一个协程进行处理
		if err != nil {
			log.Println("server: accept error:", err)
			return
		}
		go server.ServeConn(conn)
	}
}

var DefaultServer = NewServer()

func Accept(lis net.Listener) { DefaultServer.Accept(lis) }

func (server *Server) ServeConn(conn io.ReadWriteCloser) {
	//处理完一个连接之后，关闭该连接
	defer func() { _ = conn.Close() }()
	var opt Option
	//Json格式解码协议头
	if err := json.NewDecoder(conn).Decode(&opt); err != nil {
		log.Println("server: options error: ", err)
		return
	}
	//通过MagicNumber比对协议是否一致
	if opt.MagicNumber != MagicNumber {
		log.Printf("server: invalid magic number %x", opt.MagicNumber)
		return
	}
	//查找客户端要求的codec是否存在
	f := codec.NewCodecFuncMap[opt.CodecType]
	if f == nil {
		log.Printf("server: invalid codec type %s", opt.CodecType)
		return
	}
	//用指定的codec在该连接上进行数据的读取和写入
	server.serveCodec(f(conn), opt)
}

var invalidRequest = struct{}{}

func (server *Server) serveCodec(cc codec.Codec, opt Option) {
	sending := new(sync.Mutex) // 响应报文是逐个发送的，避免多个协程同时写入响应报文
	wg := new(sync.WaitGroup)  // 处理请求是并发的，关闭连接前要等待该连接的所有请求处理完毕
	for {
		req, err := server.readRequest(cc)
		if err != nil {
			if req == nil {
				break
			}
			req.h.Error = err.Error()
			server.sendResponse(cc, req.h, invalidRequest, sending)
			continue
		}
		wg.Add(1)
		go server.handleRequest(cc, req, sending, wg, opt.HandleTimeout)
	}
	//等待所有请求处理结束后，关闭该连接
	wg.Wait()
	_ = cc.Close()
}

// request 聚合读取到的Header和Body，方便处理
type request struct {
	h            *codec.Header // header
	argv, replyv reflect.Value // argv 和 replyv 分别代表参数和返回值
	//reflect.Value代表了一个任意类型的值，并提供一些方法来操作这些值
	mtype *methodType
	svc   *service
}

func (server *Server) readRequestHeader(cc codec.Codec) (*codec.Header, error) {
	var h codec.Header
	if err := cc.ReadHeader(&h); err != nil {
		if err != io.EOF && err != io.ErrUnexpectedEOF {
			log.Println("server: read header error:", err)
		}
		return nil, err
	}
	return &h, nil
}

func (server *Server) readRequest(cc codec.Codec) (*request, error) {
	h, err := server.readRequestHeader(cc)
	if err != nil {
		return nil, err
	}
	req := &request{h: h}
	req.svc, req.mtype, err = server.findService(h.ServiceMethod)
	if err != nil {
		return req, err
	}
	//创建两个入参
	req.argv = req.mtype.newArgv()
	req.replyv = req.mtype.newReplyv()
	argvi := req.argv.Interface()
	if req.argv.Type().Kind() != reflect.Ptr {
		//如果参数不是指针类型，创建一个指向参数的指针
		argvi = req.argv.Addr().Interface()
	}
	if err = cc.ReadBody(argvi); err != nil {
		log.Println("server: read body err:", err)
		return req, err
	}
	return req, nil
}

func (server *Server) sendResponse(cc codec.Codec, h *codec.Header, body interface{}, sending *sync.Mutex) {
	sending.Lock()
	defer sending.Unlock()
	if err := cc.Write(h, body); err != nil {
		log.Println("server: write response error:", err)
	}
}

func (server *Server) handleRequest(cc codec.Codec, req *request, sending *sync.Mutex, wg *sync.WaitGroup, timeout time.Duration) {
	defer wg.Done()
	called := make(chan struct{})
	sent := make(chan struct{})
	go func() {
		err := req.svc.call(req.mtype, req.argv, req.replyv)
		called <- struct{}{}
		//没有在规定的时间执行的话，called持续阻塞，不会往下走，确保只发送一次响应
		if err != nil {
			req.h.Error = err.Error()
			server.sendResponse(cc, req.h, invalidRequest, sending)
			sent <- struct{}{}
			return
		}
		server.sendResponse(cc, req.h, req.replyv.Interface(), sending)
		sent <- struct{}{}
	}()

	if timeout == 0 {
		<-called
		<-sent
		return
	}
	select {
	case <-time.After(timeout):
		req.h.Error = fmt.Sprintf("server: request handle timeout: expect within %s", timeout)
		server.sendResponse(cc, req.h, invalidRequest, sending)
	case <-called:
		<-sent
	}
}

// Register 把一个服务注册到服务器的serviceMap上
func (server *Server) Register(rcvr interface{}) error {
	s := newService(rcvr)
	if _, dup := server.serviceMap.LoadOrStore(s.name, s); dup {
		return errors.New("server: service already defined: " + s.name)
	}
	//debug, _ := server.serviceMap.Load(s.name)
	//fmt.Printf("debug: register %v\n", debug)
	return nil
}

// Register 用于给默认服务器注册服务
func Register(rcvr interface{}) error { return DefaultServer.Register(rcvr) }

// 找到对应的service
func (server *Server) findService(serviceMethod string) (svc *service, mtype *methodType, err error) {
	dot := strings.LastIndex(serviceMethod, ".")
	if dot < 0 {
		err = errors.New("server: service/method request ill-formed: " + serviceMethod)
		return
	}
	serviceName, methodName := serviceMethod[:dot], serviceMethod[dot+1:]
	//debug, _ := server.serviceMap.Load(serviceName)
	//fmt.Printf("debug: find %v\n", debug)
	svci, ok := server.serviceMap.Load(serviceName)
	if !ok {
		err = errors.New("server: can't find service " + serviceName)
		return
	}
	svc = svci.(*service)
	mtype = svc.method[methodName]
	if mtype == nil {
		err = errors.New("server: can't find method " + methodName)
	}
	return
}
