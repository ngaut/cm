package proxy

import (
	"net"
	"runtime"
	"strings"

	log "github.com/ngaut/logging"
	"github.com/wandoulabs/cm/config"
	"github.com/wandoulabs/cm/vt/tabletserver"
)

type Server struct {
	cfg *config.Config

	addr     string
	user     string
	password string

	running bool

	listener net.Listener

	nodes map[string]*Node

	schemas map[string]*Schema

	autoSchama *tabletserver.SchemaInfo
}

func NewServer(cfg *config.Config) (*Server, error) {
	s := new(Server)

	s.cfg = cfg

	s.addr = cfg.Addr
	s.user = cfg.User
	s.password = cfg.Password

	if err := s.parseNodes(); err != nil {
		return nil, err
	}

	if err := s.parseSchemas(); err != nil {
		return nil, err
	}

	s.autoSchama = tabletserver.NewSchemaInfo(128 * 1024 * 1024)

	var err error
	netProto := "tcp"
	if strings.Contains(netProto, "/") {
		netProto = "unix"
	}
	s.listener, err = net.Listen(netProto, s.addr)

	if err != nil {
		return nil, err
	}

	log.Infof("Server run MySql Protocol Listen(%s) at [%s]", netProto, s.addr)
	return s, nil
}

func (s *Server) Run() error {
	s.running = true

	for s.running {
		conn, err := s.listener.Accept()
		if err != nil {
			log.Errorf("accept error %s", err.Error())
			continue
		}

		go s.onConn(conn)
	}

	return nil
}

func (s *Server) Close() {
	s.running = false
	if s.listener != nil {
		s.listener.Close()
	}
}

func (s *Server) onConn(c net.Conn) {
	conn := s.newConn(c)

	defer func() {
		if err := recover(); err != nil {
			const size = 4096
			buf := make([]byte, size)
			buf = buf[:runtime.Stack(buf, false)]
			log.Errorf("onConn panic %v: %v\n%s", c.RemoteAddr().String(), err, buf)
		}

		conn.Close()
	}()

	if err := conn.Handshake(); err != nil {
		log.Errorf("handshake error %s", err.Error())
		c.Close()
		return
	}

	conn.Run()

}
