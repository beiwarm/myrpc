package xclient

import (
	"io"
	. "myrpc"
	"sync"
	"time"
)

// 支持负载均衡的客户端
type XClient struct {
	d       Discovery
	mode    SelectMode
	opt     *Option
	mu      sync.Mutex
	clients map[string]*Client //复用已创建好的Socket连接
}

var _ io.Closer = (*XClient)(nil)

func NewXClient(registryAddr string, mode SelectMode, opt *Option) *XClient {
	d := NewRegistryDiscovery(registryAddr, 0)
	return &XClient{d: d, mode: mode, opt: opt, clients: make(map[string]*Client)}
}

func (xc *XClient) Close() error {
	xc.mu.Lock()
	defer xc.mu.Unlock()
	for key, client := range xc.clients {
		_ = client.Close()
		delete(xc.clients, key)
	}
	return nil
}

// 如果有到这个服务器的socket了直接复用，没有就创建一个然后缓存
func (xc *XClient) dial(rpcAddr string) (*Client, error) {
	xc.mu.Lock()
	defer xc.mu.Unlock()
	client, ok := xc.clients[rpcAddr]
	if ok && !client.IsAvailable() {
		_ = client.Close()
		delete(xc.clients, rpcAddr)
		client = nil
	}
	if client == nil {
		var err error
		client, err = Dial("tcp", rpcAddr, xc.opt)
		if err != nil {
			return nil, err
		}
		xc.clients[rpcAddr] = client
	}
	return client, nil
}

func (xc *XClient) Call(serviceMethod string, args, reply interface{}, timeout time.Duration) error {
	rpcAddr, err := xc.d.Get(xc.mode)
	if err != nil {
		return err
	}
	client, err := xc.dial(rpcAddr)
	if err != nil {
		return err
	}
	return client.Call(serviceMethod, args, reply, timeout)
}
