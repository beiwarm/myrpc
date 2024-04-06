package registry

import (
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

type Registry struct {
	timeout time.Duration //注册的服务超过该时间则视为不可用
	mu      sync.Mutex
	servers map[string]*ServerItem
}

type ServerItem struct {
	Addr  string
	start time.Time //服务器上次更新的时间
}

const (
	DefaultPath    = "/myrpc/registry"
	DefaultTimeout = time.Minute * 5
)

func New(timeout time.Duration) *Registry {
	return &Registry{
		servers: make(map[string]*ServerItem),
		timeout: timeout,
	}
}

var DefaultRegistry = New(DefaultTimeout)

func (r *Registry) putServer(addr string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.servers[addr]
	if s == nil {
		r.servers[addr] = &ServerItem{
			Addr:  addr,
			start: time.Now(),
		}
	} else {
		s.start = time.Now()
	}
}

func (r *Registry) aliveServers() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var alive []string
	var toDelete []string
	// 遍历服务器列表
	for addr, s := range r.servers {
		// 如果服务器未超时，则加入alive列表
		if r.timeout == 0 || s.start.Add(r.timeout).After(time.Now()) {
			alive = append(alive, addr)
		} else {
			// 否则加入待删除列表
			toDelete = append(toDelete, addr)
		}
	}
	// 删除超时的服务器
	for _, addr := range toDelete {
		delete(r.servers, addr)
	}
	// 对alive列表排序并返回
	sort.Strings(alive)
	return alive
}

func (r *Registry) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	switch request.Method {
	case "GET": //返回所有可用服务的列表
		writer.Header().Set("MyrpcServers", strings.Join(r.aliveServers(), ","))
	case "POST": //添加服务或发送心跳
		addr := request.Header.Get("MyrpcServer")
		if addr == "" {
			writer.WriteHeader(http.StatusInternalServerError)
		}
		r.putServer(addr)
	default:
		writer.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (r *Registry) HandleHTTP(path string) {
	log.Println("registry: handle path: " + path)
	http.Handle(path, r)
}

func HandleHTTP() {
	DefaultRegistry.HandleHTTP(DefaultPath)
}

// 快速向某个注册中心发送心跳
func Heartbeat(registry, addr string, duration time.Duration) {
	var err error
	err = sendHeartbeat(registry, addr)
	go func() {
		t := time.NewTicker(duration)
		for err == nil {
			<-t.C
			err = sendHeartbeat(registry, addr)
		}
	}()
}

func sendHeartbeat(registry, addr string) error {
	log.Println(addr, "send heart beat to registry", registry)
	httpClient := &http.Client{}
	req, _ := http.NewRequest("POST", registry, nil)
	req.Header.Set("MyrpcServer", addr)
	if _, err := httpClient.Do(req); err != nil {
		log.Println("server: heart beat err:", err)
		return err
	}
	return nil
}
