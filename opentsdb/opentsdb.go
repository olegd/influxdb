package opentsdb

import (
	"bufio"
	"log"
	"net"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/influxdb/influxdb"
)

const (
	// DefaultPort represents the default OpenTSDB port.
	DefaultPort = 4242

	// DefaultDatabaseName is the default OpenTSDB database if none is specified
	DefaultDatabaseName = "opentsdb"
)

// SeriesWriter defines the interface for the destination of the data.
type SeriesWriter interface {
	WriteSeries(database, retentionPolicy string, points []influxdb.Point) (uint64, error)
}

// An InfluxDB input class to accept OpenTSDB's telnet protocol
// Each telnet command consists of a line of the form:
//   put sys.cpu.user 1356998400 42.5 host=webserver01 cpu=0
type Server struct {
	writer SeriesWriter

	database        string
	retentionpolicy string

	listener *net.TCPListener
	done     chan struct{}
	wg       sync.WaitGroup
}

func NewServer(w SeriesWriter, retpol string, db string) *Server {
	s := &Server{}

	s.writer = w
	s.retentionpolicy = retpol
	s.database = db

	s.done = make(chan struct{})

	return s
}

func (s *Server) Addr() net.Addr {
	return s.listener.Addr()
}

func (s *Server) ListenAndServe(listenAddress string) {
	var err error

	addr, err := net.ResolveTCPAddr("tcp4", listenAddress)
	if err != nil {
		log.Println("TSDBServer: ResolveTCPAddr: ", err)
		return
	}

	s.listener, err = net.ListenTCP("tcp", addr)
	if err != nil {
		log.Println("TSDBServer: Listen: ", err)
		return
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			select {
			case <-s.done:
				return
			default:
				// Keep processing.
			}
			conn, err := s.listener.Accept()
			if err != nil {
				log.Println("Error accepting: ", err.Error())
				continue
			}
			s.wg.Add(1)
			go s.HandleConnection(conn)
		}
	}()
}

func (s *Server) Close() error {
	close(s.done)
	s.wg.Wait()
	s.done = nil

	if s.listener != nil {
		err := s.listener.Close()
		s.listener = nil
		return err
	} else {
		return nil
	}
}

func (s *Server) HandleConnection(conn net.Conn) {
	reader := bufio.NewReader(conn)
	tp := textproto.NewReader(reader)

	defer conn.Close()
	defer s.wg.Done()

	for {
		select {
		case <-s.done:
			log.Println("disconnecting", conn.RemoteAddr())
			return
		default:
		}

		line, err := tp.ReadLine()
		if err != nil {
			return
		}

		inputStrs := strings.Fields(line)

		if len(inputStrs) == 1 && inputStrs[0] == "version" {
			conn.Write([]byte("InfluxDB TSDB proxy"))
			continue
		}

		if len(inputStrs) < 4 || inputStrs[0] != "put" {
			log.Println("TSDBServer: malformed line, skipping: ", line)
			continue
		}

		name := inputStrs[1]
		tsStr := inputStrs[2]
		valueStr := inputStrs[3]
		tagStrs := inputStrs[4:]

		var t time.Time
		ts, err := strconv.ParseInt(tsStr, 10, 64)
		if err != nil {
			log.Println("TSDBServer: malformed timestamp, skipping: ", tsStr)
		}

		switch len(tsStr) {
		case 10:
			t = time.Unix(ts, 0)
			break
		case 13:
			t = time.Unix(ts/1000, (ts%1000)*1000)
			break
		default:
			log.Println("TSDBServer: timestamp must be 10 or 13 chars, skipping: ", tsStr)
			continue
		}

		tags := make(map[string]string)
		for t := range tagStrs {
			parts := strings.SplitN(tagStrs[t], "=", 2)
			if len(parts) != 2 {
				log.Println("TSDBServer: malformed tag data", tagStrs[t])
				continue
			}
			k := parts[0]

			tags[k] = parts[1]
		}

		fields := make(map[string]interface{})
		fields["value"], err = strconv.ParseFloat(valueStr, 64)
		if err != nil {
			log.Println("TSDBServer: could not parse value as float: ", valueStr)
			continue
		}

		p := influxdb.Point{
			Name:      name,
			Tags:      tags,
			Timestamp: t,
			Fields:    fields,
		}

		_, err = s.writer.WriteSeries(s.database, s.retentionpolicy, []influxdb.Point{p})
		if err != nil {
			log.Println("TSDB cannot write data: ", err)
			continue
		}
	}
}
