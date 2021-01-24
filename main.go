package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/signal"
	"path"
	"strconv"
	"syscall"
	"time"

	mcNet "github.com/Tnze/go-mc/net"
	mcPkt "github.com/Tnze/go-mc/net/packet"
)

const (
	handshaking = iota
	status
	login
	play
)

var goalAddress = flag.String("backend", "192.168.88.128:25565", "Address of the backend server to be proxied")
var listenAddress = flag.String("listen", ":25565", "Address to listen on for client connections")
var file = flag.String("file", "packets.json", "Path to json file for replaying and saving")
var doReplay = flag.Bool("replay", false, "bool to activate replaying of given file")

func main() {
	flag.Parse()

	if *doReplay {
		replay()
	} else {
		record()
	}
}

type SerializableSession struct {
	Packets []recordedPacket
	LoginX  mcPkt.Double
	LoginY  mcPkt.Double
	LoginZ  mcPkt.Double
}

type Session struct {
	StartTime time.Time
	Client    *mcNet.Conn
	Server    *mcNet.Conn
	State     int
	SerializableSession
}

func NewSession(listener *mcNet.Listener) (sess Session, err error) {
	client, err := listener.Accept()
	if err != nil {
		return sess, err
	}
	log.Printf("Accepted connection from %s\n", client.Socket.RemoteAddr().String())
	sess.Client = &client
	sess.StartTime = time.Now().UTC()

	server, err := mcNet.DialMC(*goalAddress)
	if err != nil {
		_ = client.Close()
		return sess, err
	}
	log.Printf("Connected to backend on %s\n", *goalAddress)
	sess.Server = server

	return sess, err
}

func (s *Session) SaveToFile(filename string) {
	jsonPackets, err := json.Marshal(s.SerializableSession)
	if err != nil {
		log.Printf("Unable to marshal recorded packets to json: %v\n", err)
		return
	}

	resultFile, err := os.Create(filename)
	if err != nil {
		log.Printf("Unable to create file for recorded packets: %v\n", err)
		return
	}
	defer resultFile.Close()

	_, err = resultFile.Write(jsonPackets)
	if err != nil {
		log.Printf("Unable to write recorded packets to file: %v\n", err)
	}
}

func (s *Session) Close() {
	_ = s.Client.Close()
	_ = s.Server.Close()
}

func (s *Session) StreamBidirectional() {
	errs := make(chan error, 2)
	closer := make(chan interface{}, 2)
	go s.ClientToServer(errs, closer)
	go s.ServerToClient(errs, closer)

	<-errs
	closer <- struct{}{}
	closer <- struct{}{}
	s.Close()
}

func (s *Session) ClientToServer(errs chan error, closer chan interface{}) {
	for {
		select {
		case <-closer:
			return
		default:
			packet, err := s.Client.ReadPacket()
			if err != nil {
				if errors.Is(err, io.EOF) {
					errs <- err
					break
				}
				log.Printf("Unable to read packet from client: %v\n", err)
				continue
			}

			go s.proccessServerbound(packet)

			if err := s.Server.WritePacket(packet); err != nil {
				if errors.Is(err, io.EOF) {
					errs <- err
					break
				}
				log.Printf("Unable to send packet to server: %v\n", err)
			}
		}
	}
}

func (s *Session) proccessServerbound(packet mcPkt.Packet) {
	if s.State == play {
		switch packet.ID {
		case 0x10:
			return
		case 0x00:
			return
		}
	}

	s.setStateFromServerbound(packet.ID)

	recorded := recordedPacket{
		Packet:    packet,
		RelatTime: time.Since(s.StartTime),
	}

	s.Packets = append(s.Packets, recorded)
}

func (s *Session) setStateFromServerbound(id int32) {
	if s.State == handshaking {
		if id == 0x00 {
			s.State = login
			log.Printf("Switched to login state\n")
		}
	}
}

func (s *Session) ServerToClient(errs chan error, closer chan interface{}) {
	for {
		select {
		case <-closer:
			return
		default:
			packet, err := s.Server.ReadPacket()
			if err != nil {
				if errors.Is(err, io.EOF) {
					errs <- err
					break
				}
				log.Printf("Unable to read packet from server: %v\n", err)
				continue
			}

			s.proccessClientbound(packet)

			if err := s.Client.WritePacket(packet); err != nil {
				if errors.Is(err, io.EOF) {
					errs <- err
					break
				}
				log.Printf("Unable to send packet to client: %v\n", err)
			}
		}
	}
}

func (s *Session) proccessClientbound(packet mcPkt.Packet) {
	s.setStateFromClientbound(packet.ID)

	if s.State == play {
		if packet.ID == 0x34 && s.LoginX == 0 && s.LoginY == 0 && s.LoginZ == 0 {
			if err := packet.Scan(&s.LoginX, &s.LoginY, &s.LoginZ); err != nil {
				log.Printf("Unable to parse login position from packet: %v\n", err)
				return
			}
		}
	}
}

func (s *Session) setStateFromClientbound(id int32) {
	if s.State == login {
		if id == 0x02 {
			s.State = play
			log.Printf("Switched to play state\n")
		}
	}
}

type recordedPacket struct {
	Packet    mcPkt.Packet
	RelatTime time.Duration
}

type proxy struct {
	Listener *mcNet.Listener
	Sessions []*Session
}

func newProxy(address string) (p proxy, err error) {
	listener, err := mcNet.ListenMC(address)
	p.Listener = listener

	return p, err
}

func (p *proxy) handleSessions() {
	for {
		session, err := NewSession(p.Listener)
		if err != nil {
			log.Printf("Unable to create new session: %v\n", err)
			continue
		}
		p.Sessions = append(p.Sessions, &session)
		go session.StreamBidirectional()
	}
}

func (p *proxy) Close() {
	for _, sess := range p.Sessions {
		sess.Close()
	}
}

func (p *proxy) SaveSessions() {
	for i, sess := range p.Sessions {
		prefix := strconv.Itoa(i)
		dir, filename := path.Split(*file)
		newName := prefix + filename
		newPath := dir + newName
		sess.SaveToFile(newPath)
	}
}

func record() {
	p, err := newProxy(*listenAddress)
	if err != nil {
		log.Fatalf("Unable to create new proxy: %v\n", err)
	}

	go p.handleSessions()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	<-signals
	p.Close()
	p.SaveSessions()
}

func replay() {
	session := NewReplaySession()
	go session.respondToServer()
	session.replayPackets()
}

type ReplaySession struct {
	Session
	wasPorted bool
}

func NewReplaySession() (sess ReplaySession) {
	sess.ReadPacketsFromFile(*file)

	server, err := mcNet.DialMC(*goalAddress)
	if err != nil {
		log.Fatalf("Unable to connect to server: %v\n", err)
	}
	sess.Server = server
	sess.StartTime = time.Now().UTC()

	return sess
}

func (s *ReplaySession) ReadPacketsFromFile(filename string) {
	file, err := os.Open(filename)
	if err != nil {
		log.Fatalf("Unable to open recorded packets file: %v\n", err)
	}
	defer file.Close()

	data, err := ioutil.ReadAll(file)
	if err != nil {
		log.Fatalf("Unable to read recorded packets file: %v\n", err)
	}

	if err := json.Unmarshal(data, &s.SerializableSession); err != nil {
		log.Fatalf("Unable to unmarshal recorded packets from file: %v\n", err)
	}
}

func (s *ReplaySession) respondToServer() {
	for {
		packet, err := s.Server.ReadPacket()
		if err != nil {
			break
		}

		if s.State == play {
			if packet.ID == 0x1F {
				var keepID mcPkt.Long
				if err := packet.Scan(&keepID); err != nil {
					continue
				}

				response := mcPkt.Marshal(0x10, keepID)
				if err := s.Server.WritePacket(response); err != nil {
					continue
				}
				log.Printf("Responded to keepAlive\n")
			}

			if packet.ID == 0x34 {
				var x, y, z mcPkt.Double
				var yaw, pitch mcPkt.Float
				var flags mcPkt.Byte
				var tpID mcPkt.VarInt
				if err := packet.Scan(&x, &y, &z, &yaw, &pitch, &flags, &tpID); err != nil {
					continue
				}

				response := mcPkt.Marshal(0x00, tpID)
				if err := s.Server.WritePacket(response); err != nil {
					continue
				}
				log.Printf("Responded with teleport confirm\n")
			}
		}

		s.setStateFromClientbound(packet.ID)
	}
}

func (s *ReplaySession) replayPackets() {
	for _, packet := range s.Packets {
		if s.State == play {
			if !s.wasPorted && packet.Packet.ID >= 0x12 && packet.Packet.ID <= 0x16 {
				s.portToLogin()
			}
		}

		waitTime := packet.RelatTime - time.Since(s.StartTime)
		if waitTime > 0 {
			time.Sleep(waitTime)
		}

		s.setStateFromServerbound(packet.Packet.ID)
		log.Printf("Sending packet: %v\n", packet)
		if err := s.Server.WritePacket(packet.Packet); err != nil {
			break
		}
	}

	if err := s.Server.Close(); err != nil {
		log.Fatalf("Unable to close connection to server")
	}
}

func (s *ReplaySession) portToLogin() {
	var message = mcPkt.String(fmt.Sprintf("/teleport %f %f %f", s.LoginX, s.LoginY, s.LoginZ))
	packet := mcPkt.Marshal(0x03, message)
	if err := s.Server.WritePacket(packet); err != nil {
		log.Printf("Unable to send teleport packet to server: %v\n", err)
		return
	}

	s.wasPorted = true
}
