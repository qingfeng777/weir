// Copyright 2015 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"context"
	"math/rand"
	"net"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/pingcap-incubator/weir/pkg/config"
	"github.com/pingcap/errors"
	"github.com/pingcap/parser/mysql"
	"github.com/pingcap/parser/terror"
	"github.com/pingcap/tidb/errno"
	"github.com/pingcap/tidb/metrics"
	"github.com/pingcap/tidb/util/fastrand"
	"github.com/pingcap/tidb/util/logutil"
	"go.uber.org/zap"
)

var (
	errUnknownFieldType        = terror.ClassServer.New(errno.ErrUnknownFieldType, errno.MySQLErrName[errno.ErrUnknownFieldType])
	errInvalidSequence         = terror.ClassServer.New(errno.ErrInvalidSequence, errno.MySQLErrName[errno.ErrInvalidSequence])
	errInvalidType             = terror.ClassServer.New(errno.ErrInvalidType, errno.MySQLErrName[errno.ErrInvalidType])
	errNotAllowedCommand       = terror.ClassServer.New(errno.ErrNotAllowedCommand, errno.MySQLErrName[errno.ErrNotAllowedCommand])
	errAccessDenied            = terror.ClassServer.New(errno.ErrAccessDenied, errno.MySQLErrName[errno.ErrAccessDenied])
	errConCount                = terror.ClassServer.New(errno.ErrConCount, errno.MySQLErrName[errno.ErrConCount])
	errSecureTransportRequired = terror.ClassServer.New(errno.ErrSecureTransportRequired, errno.MySQLErrName[errno.ErrSecureTransportRequired])
)

// DefaultCapability is the capability of the server when it is created using the default configuration.
// When server is configured with SSL, the server will have extra capabilities compared to DefaultCapability.
const defaultCapability = mysql.ClientLongPassword | mysql.ClientLongFlag |
	mysql.ClientConnectWithDB | mysql.ClientProtocol41 |
	mysql.ClientTransactions | mysql.ClientSecureConnection | mysql.ClientFoundRows |
	mysql.ClientMultiStatements | mysql.ClientMultiResults | mysql.ClientLocalFiles |
	mysql.ClientConnectAtts | mysql.ClientPluginAuth | mysql.ClientInteractive

type Server struct {
	listener   net.Listener
	cfg        *config.Proxy
	baseConnID uint32
	capability uint32
	tlsConfig  unsafe.Pointer // *tls.Config
	driver     IDriver
}

// NewServer creates a new Server.
func NewServer(cfg *config.Proxy, driver IDriver) (*Server, error) {
	// TODO: handle the unset fields
	s := &Server{
		cfg:    cfg,
		driver: driver,
		//concurrentLimiter: NewTokenLimiter(cfg.TokenLimit),
		//clients:           make(map[uint32]*clientConn),
	}

	// TODO(eastfisher): set tlsConfig

	setSystemTimeZoneVariable()

	s.initCapability()

	if err := s.initListener(); err != nil {
		return nil, err
	}

	// TODO(eastfisher): init status http server

	// Init rand seed for randomBuf()
	rand.Seed(time.Now().UTC().UnixNano())

	return s, nil
}

func (s *Server) initCapability() {
	s.capability = defaultCapability
	if s.tlsConfig != nil {
		s.capability |= mysql.ClientSSL
	}
}

// TODO(eastfisher): support unix socket and proxyprotocol
func (s *Server) initListener() error {
	listener, err := net.Listen("tcp", s.cfg.ProxyServer.Addr)
	if err != nil {
		return err
	}
	s.listener = listener
	return nil
}

func (s *Server) Run() error {
	metrics.ServerEventCounter.WithLabelValues(metrics.EventStart).Inc()

	// TODO(eastfisher): startStatusHTTP()

	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if opErr, ok := err.(*net.OpError); ok {
				if opErr.Err.Error() == "use of closed network connection" {
					return nil
				}
			}

			// TODO(eastfisher): support PROXY protocol

			logutil.BgLogger().Error("accept failed", zap.Error(err))
			return errors.Trace(err)
		}

		clientConn := s.newClientConn(conn)
		go s.onConn(clientConn)
	}
}

func (s *Server) GetNextConnID() uint32 {
	return atomic.AddUint32(&s.baseConnID, 1)
}

func (s *Server) onConn(conn *clientConn) {
	ctx := logutil.WithConnID(context.Background(), conn.connectionID)
	if err := conn.handshake(ctx); err != nil {
		// Some keep alive services will send request to TiDB and disconnect immediately.
		// So we only record metrics.
		metrics.HandShakeErrorCounter.Inc()
		err = conn.Close()
		terror.Log(errors.Trace(err))
		return
	}

	logutil.Logger(ctx).Info("new connection", zap.String("remoteAddr", conn.bufReadConn.RemoteAddr().String()))

	defer func() {
		logutil.Logger(ctx).Info("connection closed")
	}()

	// TODO(eastfisher): record ConnGauge

	conn.Run(ctx)
}

func (s *Server) newClientConn(conn net.Conn) *clientConn {
	cc := newClientConn(s)
	cc.setConn(conn)
	cc.salt = fastrand.Buf(20)
	return cc
}

// TODO: implement this function
func (s *Server) checkConnectionCount() error {
	return nil
}

// TODO: implement this function
func (s *Server) isUnixSocket() bool {
	return false
}

// Close closes the server.
// TODO: implement this function
func (s *Server) Close() {
	if s.listener != nil {
		err := s.listener.Close()
		terror.Log(errors.Trace(err))
		s.listener = nil
	}
}
