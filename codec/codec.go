package codec

import "io"

// Header 每次RPC调用的方法名和错误放在Header里，参数和返回值抽象为Body
type Header struct {
	Seq           uint64 // id
	ServiceMethod string // format "Service.Method"
	Error         string
}

// 消息编解码器
type Codec interface {
	io.Closer
	ReadHeader(*Header) error
	ReadBody(interface{}) error
	Write(*Header, interface{}) error
}

// Codec的类型
type Type string

const (
	GobType  Type = "application/gob"
	JsonType Type = "application/json" // not implemented
)

// 可以实例化出一个Codec对象的构造函数
type NewCodecFunc func(io.ReadWriteCloser) Codec

var NewCodecFuncMap map[Type]NewCodecFunc

func init() {
	NewCodecFuncMap = make(map[Type]NewCodecFunc)
	NewCodecFuncMap[GobType] = NewGobCodec
}
