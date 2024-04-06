package main

import (
	"log"
	"myrpc"
	"myrpc/registry"
	"myrpc/xclient"
	"net"
	"net/http"
	"sync"
	"time"
)

type Foo int

type Args struct{ Num1, Num2 int }

func (f Foo) Sum(args Args, reply *int) error {
	*reply = args.Num1 + args.Num2
	return nil
}

func startRegistry(group *sync.WaitGroup) {
	l, _ := net.Listen("tcp", ":9999")
	registry.HandleHTTP()
	group.Done()
	log.Println("registry: start")
	_ = http.Serve(l, nil)
}

func startServer(registryAddr string, group *sync.WaitGroup) {
	var foo Foo
	// 0是随机分配一个端口，开始监听连接
	l, err := net.Listen("tcp", ":0")
	if err != nil {
		log.Fatal("network error:", err)
	}
	server := myrpc.NewServer()
	err = server.Register(&foo)
	if err != nil {
		log.Fatal("register error:", err)
	}
	registry.Heartbeat(registryAddr, l.Addr().String(), registry.DefaultTimeout-time.Minute)
	group.Done()
	server.Accept(l)
}

func main() {
	log.SetFlags(0)
	registryAddr := "http://localhost:9999" + registry.DefaultPath
	var wg sync.WaitGroup
	wg.Add(1)
	go startRegistry(&wg)
	wg.Wait()
	time.Sleep(time.Second)
	wg.Add(2)
	go startServer(registryAddr, &wg)
	go startServer(registryAddr, &wg)
	wg.Wait()
	time.Sleep(time.Second)
	xc := xclient.NewXClient(registryAddr, xclient.RandomSelect, myrpc.DefaultOption)
	defer func() { xc.Close() }()
	var calls sync.WaitGroup
	for i := 0; i < 5; i++ {
		calls.Add(1)
		go func(i int) {
			defer calls.Done()
			var reply int
			args := &Args{
				Num1: i,
				Num2: i * i,
			}
			err := xc.Call("Foo.Sum", args, &reply, time.Second*5)
			if err != nil {
				log.Fatal("call Foo.Sum error:", err)
			}
			log.Printf("call Foo.Sum success: %d + %d = %d", args.Num1, args.Num2, reply)
		}(i)
	}
	calls.Wait()
}
