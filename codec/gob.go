package codec

import (
	"bufio"
	"encoding/gob"
	"io"
	"log"
)

type GobCodec struct {
	conn io.ReadWriteCloser //与客户端建立的tcp连接
	buf  *bufio.Writer      //数据先写入buffer，再一次性写入socket，减少系统调用次数
	dec  *gob.Decoder
	enc  *gob.Encoder
}

var _ Codec = (*GobCodec)(nil)

func NewGobCodec(conn io.ReadWriteCloser) Codec {
	buf := bufio.NewWriter(conn)
	//在原本的Writer上加了一个缓冲区，缓冲区满了或者手动刷新时，数据会写入conn
	//默认4kb
	//读取操作通常是阻塞的，一直在等待有数据可读，不会频繁调用底层接口，读取操作本身的耗时并不明显，所以读数据不用加buffer
	return &GobCodec{
		conn: conn,
		buf:  buf,
		dec:  gob.NewDecoder(conn),
		enc:  gob.NewEncoder(buf),
	}
}

func (c *GobCodec) ReadHeader(h *Header) error {
	return c.dec.Decode(h)
}

func (c *GobCodec) ReadBody(body interface{}) error {
	return c.dec.Decode(body)
}

func (c *GobCodec) Write(h *Header, body interface{}) (err error) {
	defer func() {
		//fmt.Printf("debug: writed %v %v\n", h, body)
		//写入一对Header-Body后刷一次缓冲区，减少一半的系统调用
		_ = c.buf.Flush()
		//如果某次写入操作报错了，该连接无法正常使用了，关闭连接
		if err != nil {
			_ = c.Close()
		}
	}()
	if err = c.enc.Encode(h); err != nil {
		log.Println("server: error encoding header:", err)
		return err
	}
	if err = c.enc.Encode(body); err != nil {
		log.Println("server: error encoding body:", err)
		return err
	}
	return nil
}

func (c *GobCodec) Close() error {
	return c.conn.Close()
}
