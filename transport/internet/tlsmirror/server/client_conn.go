package server

import (
	"bytes"
	"context"
	gonet "net"
	"time"

	"github.com/v2fly/v2ray-core/v5/common/net"
	"github.com/v2fly/v2ray-core/v5/transport/internet"
	"github.com/v2fly/v2ray-core/v5/transport/internet/tlsmirror"
	"github.com/v2fly/v2ray-core/v5/transport/internet/tlsmirror/mirrorcommon"
	"github.com/v2fly/v2ray-core/v5/transport/internet/tlsmirror/mirrorcrypto"
)

type clientConnState struct {
	ctx     context.Context
	done    context.CancelFunc
	handler internet.ConnHandler

	mirrorConn tlsmirror.InsertableTLSConn
	localAddr  net.Addr
	remoteAddr net.Addr

	activated bool
	decryptor *mirrorcrypto.Decryptor
	encryptor *mirrorcrypto.Encryptor

	primaryKey []byte

	readPipe   chan []byte
	readBuffer *bytes.Buffer

	protocolVersion [2]byte
}

func (s *clientConnState) GetConnectionContext() context.Context {
	return s.ctx
}

func (s *clientConnState) Read(b []byte) (n int, err error) {
	if s.readBuffer != nil {
		n, _ = s.readBuffer.Read(b)
		if n > 0 {
			return n, nil
		}
		s.readBuffer = nil
	}

	select {
	case <-s.ctx.Done():
		return 0, s.ctx.Err()
	case data := <-s.readPipe:
		s.readBuffer = bytes.NewBuffer(data)
		n, err = s.readBuffer.Read(b)
		if err != nil {
			return 0, err
		}
		return n, nil
	}
}

func (s *clientConnState) Write(b []byte) (n int, err error) {
	err = s.WriteMessage(b)
	if err != nil {
		return 0, err
	}
	n = len(b)
	return n, nil
}

func (s *clientConnState) Close() error {
	s.done()
	return nil
}

func (s *clientConnState) LocalAddr() gonet.Addr {
	return s.remoteAddr
}

func (s *clientConnState) RemoteAddr() gonet.Addr {
	return s.remoteAddr
}

func (s *clientConnState) SetDeadline(t time.Time) error {
	return nil
}

func (s *clientConnState) SetReadDeadline(t time.Time) error {
	return nil
}

func (s *clientConnState) SetWriteDeadline(t time.Time) error {
	return nil
}

func (s *clientConnState) onC2SMessage(message *tlsmirror.TLSRecord) (drop bool, ok error) {
	if message.RecordType == mirrorcommon.TLSRecord_RecordType_application_data {
		if s.decryptor == nil {
			clientRandom, serverRandom, err := s.mirrorConn.GetHandshakeRandom()
			if err != nil {
				newError("failed to get handshake random").Base(err).AtWarning().WriteToLog()
				return false, nil
			}

			{
				encryptionKey, nonceMask, err := mirrorcrypto.DeriveEncryptionKey(s.primaryKey, clientRandom, serverRandom, ":s2c")
				if err != nil {
					newError("failed to derive C2S encryption key").Base(err).AtWarning().WriteToLog()
					return false, nil
				}
				s.decryptor = mirrorcrypto.NewDecryptor(encryptionKey, nonceMask)
			}

			{
				encryptionKey, nonceMask, err := mirrorcrypto.DeriveEncryptionKey(s.primaryKey, clientRandom, serverRandom, ":c2s")
				if err != nil {
					newError("failed to derive S2C encryption key").Base(err).AtWarning().WriteToLog()
					return false, nil
				}
				s.encryptor = mirrorcrypto.NewEncryptor(encryptionKey, nonceMask)
			}
			s.protocolVersion = message.LegacyProtocolVersion

			if !s.activated {
				s.handler(s)
				s.activated = true
			}
		}
	}
	return false, ok
}

func (s *clientConnState) onS2CMessage(message *tlsmirror.TLSRecord) (drop bool, ok error) {
	if message.RecordType == mirrorcommon.TLSRecord_RecordType_application_data {
		if s.encryptor == nil {
			return false, nil
		}
		buffer := make([]byte, 0, len(message.Fragment)-s.encryptor.NonceSize())
		buffer, err := s.decryptor.Open(buffer, message.Fragment)
		if err != nil {
			return false, nil
		}

		s.readPipe <- buffer
		return true, nil
	}
	return false, ok
}

func (s *clientConnState) WriteMessage(message []byte) error {
	buffer := make([]byte, 0, len(message)+s.encryptor.NonceSize())
	buffer, err := s.encryptor.Seal(buffer, message)
	if err != nil {
		return newError("failed to encrypt message").Base(err)
	}
	record := tlsmirror.TLSRecord{
		RecordType:            mirrorcommon.TLSRecord_RecordType_application_data,
		LegacyProtocolVersion: s.protocolVersion,
		RecordLength:          uint16(len(buffer)),
		Fragment:              buffer,
	}
	return s.mirrorConn.InsertC2SMessage(&record)
}
