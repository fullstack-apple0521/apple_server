package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/exec"
	"time"

	"github.com/aler9/gortsplib"
	"github.com/aler9/sdp/v3"
	"gopkg.in/alecthomas/kingpin.v2"
)

var Version = "v0.0.0"

const (
	pprofAddress = ":9999"
)

type logDestination int

const (
	logDestinationStdout logDestination = iota
	logDestinationFile
)

type programEvent interface {
	isProgramEvent()
}

type programEventMetrics struct {
	res chan *metricsData
}

func (programEventMetrics) isProgramEvent() {}

type programEventClientNew struct {
	nconn net.Conn
}

func (programEventClientNew) isProgramEvent() {}

type programEventClientClose struct {
	done   chan struct{}
	client *client
}

func (programEventClientClose) isProgramEvent() {}

type programEventClientDescribe struct {
	client *client
	path   string
}

func (programEventClientDescribe) isProgramEvent() {}

type programEventClientAnnounce struct {
	res       chan error
	client    *client
	path      string
	sdpText   []byte
	sdpParsed *sdp.SessionDescription
}

func (programEventClientAnnounce) isProgramEvent() {}

type programEventClientSetupPlay struct {
	res     chan error
	client  *client
	path    string
	trackId int
}

func (programEventClientSetupPlay) isProgramEvent() {}

type programEventClientSetupRecord struct {
	res    chan error
	client *client
}

func (programEventClientSetupRecord) isProgramEvent() {}

type programEventClientPlay1 struct {
	res    chan error
	client *client
}

func (programEventClientPlay1) isProgramEvent() {}

type programEventClientPlay2 struct {
	done   chan struct{}
	client *client
}

func (programEventClientPlay2) isProgramEvent() {}

type programEventClientPlayStop struct {
	done   chan struct{}
	client *client
}

func (programEventClientPlayStop) isProgramEvent() {}

type programEventClientRecord struct {
	done   chan struct{}
	client *client
}

func (programEventClientRecord) isProgramEvent() {}

type programEventClientRecordStop struct {
	done   chan struct{}
	client *client
}

func (programEventClientRecordStop) isProgramEvent() {}

type programEventClientFrameUdp struct {
	addr       *net.UDPAddr
	streamType gortsplib.StreamType
	buf        []byte
}

func (programEventClientFrameUdp) isProgramEvent() {}

type programEventClientFrameTcp struct {
	path       string
	trackId    int
	streamType gortsplib.StreamType
	buf        []byte
}

func (programEventClientFrameTcp) isProgramEvent() {}

type programEventSourceReady struct {
	source *source
}

func (programEventSourceReady) isProgramEvent() {}

type programEventSourceNotReady struct {
	source *source
}

func (programEventSourceNotReady) isProgramEvent() {}

type programEventSourceFrame struct {
	source     *source
	trackId    int
	streamType gortsplib.StreamType
	buf        []byte
}

func (programEventSourceFrame) isProgramEvent() {}

type programEventTerminate struct{}

func (programEventTerminate) isProgramEvent() {}

type program struct {
	conf             *conf
	logFile          *os.File
	metrics          *metrics
	serverRtsp       *serverTcp
	serverRtp        *serverUdp
	serverRtcp       *serverUdp
	sources          []*source
	clients          map[*client]struct{}
	udpClientsByAddr map[udpClientAddr]*udpClient
	paths            map[string]*path
	cmds             []*exec.Cmd
	publisherCount   int
	readerCount      int

	events chan programEvent
	done   chan struct{}
}

func newProgram(args []string, stdin io.Reader) (*program, error) {
	k := kingpin.New("rtsp-simple-server",
		"rtsp-simple-server "+Version+"\n\nRTSP server.")

	argVersion := k.Flag("version", "print version").Bool()
	argConfPath := k.Arg("confpath", "path to a config file. The default is rtsp-simple-server.yml. Use 'stdin' to read config from stdin").Default("rtsp-simple-server.yml").String()

	kingpin.MustParse(k.Parse(args))

	if *argVersion == true {
		fmt.Println(Version)
		os.Exit(0)
	}

	conf, err := loadConf(*argConfPath, stdin)
	if err != nil {
		return nil, err
	}

	p := &program{
		conf:             conf,
		clients:          make(map[*client]struct{}),
		udpClientsByAddr: make(map[udpClientAddr]*udpClient),
		paths:            make(map[string]*path),
		events:           make(chan programEvent),
		done:             make(chan struct{}),
	}

	if _, ok := p.conf.logDestinationsParsed[logDestinationFile]; ok {
		p.logFile, err = os.OpenFile(p.conf.LogFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			log.Fatal("ERR:", err)
		}
	}

	p.log("rtsp-simple-server %s", Version)

	for name, confp := range conf.Paths {
		if name == "all" {
			continue
		}

		p.paths[name] = newPath(p, name, confp, true)

		if confp.Source != "record" {
			s := newSource(p, name, confp)
			p.sources = append(p.sources, s)
			p.paths[name].publisher = s
		}
	}

	if conf.Metrics {
		p.metrics = newMetrics(p)
	}

	if conf.Pprof {
		go func(mux *http.ServeMux) {
			p.log("[pprof] opened on " + pprofAddress)
			panic((&http.Server{
				Addr:    pprofAddress,
				Handler: mux,
			}).ListenAndServe())
		}(http.DefaultServeMux)
		http.DefaultServeMux = http.NewServeMux()
	}

	p.serverRtp, err = newServerUdp(p, conf.RtpPort, gortsplib.StreamTypeRtp)
	if err != nil {
		return nil, err
	}

	p.serverRtcp, err = newServerUdp(p, conf.RtcpPort, gortsplib.StreamTypeRtcp)
	if err != nil {
		return nil, err
	}

	p.serverRtsp, err = newServerTcp(p)
	if err != nil {
		return nil, err
	}

	for name, confp := range conf.Paths {
		if confp.RunOnInit != "" {
			onInitCmd := exec.Command("/bin/sh", "-c", confp.RunOnInit)
			onInitCmd.Env = append(os.Environ(),
				"RTSP_SERVER_PATH="+name,
			)
			onInitCmd.Stdout = os.Stdout
			onInitCmd.Stderr = os.Stderr
			err := onInitCmd.Start()
			if err != nil {
				p.log("ERR: %s", err)
			}

			p.cmds = append(p.cmds, onInitCmd)
		}
	}

	if p.metrics != nil {
		go p.metrics.run()
	}
	go p.serverRtp.run()
	go p.serverRtcp.run()
	go p.serverRtsp.run()
	for _, s := range p.sources {
		go s.run()
	}
	go p.run()

	return p, nil
}

func (p *program) log(format string, args ...interface{}) {
	line := fmt.Sprintf("[%d/%d/%d] "+format, append([]interface{}{len(p.clients),
		p.publisherCount, p.readerCount}, args...)...)

	if _, ok := p.conf.logDestinationsParsed[logDestinationStdout]; ok {
		log.Println(line)
	}
	if _, ok := p.conf.logDestinationsParsed[logDestinationFile]; ok {
		p.logFile.WriteString(line + "\n")
	}
}

func (p *program) run() {
	checkPathsTicker := time.NewTicker(5 * time.Second)
	defer checkPathsTicker.Stop()

outer:
	for {
		select {
		case <-checkPathsTicker.C:
			for _, path := range p.paths {
				path.check()
			}

		case rawEvt := <-p.events:
			switch evt := rawEvt.(type) {
			case programEventMetrics:
				evt.res <- &metricsData{
					clientCount:    len(p.clients),
					publisherCount: p.publisherCount,
					readerCount:    p.readerCount,
				}

			case programEventClientNew:
				c := newClient(p, evt.nconn)
				p.clients[c] = struct{}{}
				c.log("connected")

			case programEventClientClose:
				delete(p.clients, evt.client)

				if evt.client.pathName != "" {
					if path, ok := p.paths[evt.client.pathName]; ok {
						if path.publisher == evt.client {
							path.publisherRemove()

							if !path.permanent {
								delete(p.paths, evt.client.pathName)
							}
						}
					}
				}

				evt.client.log("disconnected")
				close(evt.done)

			case programEventClientDescribe:
				path, ok := p.paths[evt.path]
				if !ok {
					evt.client.describeRes <- describeRes{nil, fmt.Errorf("no one is publishing on path '%s'", evt.path)}
					continue
				}

				path.describe(evt.client)

			case programEventClientAnnounce:
				if path, ok := p.paths[evt.path]; ok {
					if path.publisher != nil {
						evt.res <- fmt.Errorf("someone is already publishing on path '%s'", evt.path)
						continue
					}

				} else {
					p.paths[evt.path] = newPath(p, evt.path, p.findConfForPath(evt.path), false)
				}

				p.paths[evt.path].publisher = evt.client
				p.paths[evt.path].publisherSdpText = evt.sdpText
				p.paths[evt.path].publisherSdpParsed = evt.sdpParsed

				evt.client.pathName = evt.path
				evt.client.state = clientStateAnnounce
				evt.res <- nil

			case programEventClientSetupPlay:
				path, ok := p.paths[evt.path]
				if !ok || !path.publisherReady {
					evt.res <- fmt.Errorf("no one is publishing on path '%s'", evt.path)
					continue
				}

				if evt.trackId >= len(path.publisherSdpParsed.MediaDescriptions) {
					evt.res <- fmt.Errorf("track %d does not exist", evt.trackId)
					continue
				}

				evt.client.pathName = evt.path
				evt.client.state = clientStatePrePlay
				evt.res <- nil

			case programEventClientSetupRecord:
				evt.client.state = clientStatePreRecord
				evt.res <- nil

			case programEventClientPlay1:
				path, ok := p.paths[evt.client.pathName]
				if !ok || !path.publisherReady {
					evt.res <- fmt.Errorf("no one is publishing on path '%s'", evt.client.pathName)
					continue
				}

				if len(evt.client.streamTracks) == 0 {
					evt.res <- fmt.Errorf("no tracks have been setup")
					continue
				}

				evt.res <- nil

			case programEventClientPlay2:
				p.readerCount += 1
				evt.client.state = clientStatePlay
				close(evt.done)

			case programEventClientPlayStop:
				p.readerCount -= 1
				evt.client.state = clientStatePrePlay
				close(evt.done)

			case programEventClientRecord:
				p.publisherCount += 1
				evt.client.state = clientStateRecord

				if evt.client.streamProtocol == gortsplib.StreamProtocolUdp {
					for trackId, track := range evt.client.streamTracks {
						key := makeUdpClientAddr(evt.client.ip(), track.rtpPort)
						p.udpClientsByAddr[key] = &udpClient{
							client:     evt.client,
							trackId:    trackId,
							streamType: gortsplib.StreamTypeRtp,
						}

						key = makeUdpClientAddr(evt.client.ip(), track.rtcpPort)
						p.udpClientsByAddr[key] = &udpClient{
							client:     evt.client,
							trackId:    trackId,
							streamType: gortsplib.StreamTypeRtcp,
						}
					}
				}

				p.paths[evt.client.pathName].publisherSetReady()
				close(evt.done)

			case programEventClientRecordStop:
				p.publisherCount -= 1
				evt.client.state = clientStatePreRecord
				if evt.client.streamProtocol == gortsplib.StreamProtocolUdp {
					for _, track := range evt.client.streamTracks {
						key := makeUdpClientAddr(evt.client.ip(), track.rtpPort)
						delete(p.udpClientsByAddr, key)

						key = makeUdpClientAddr(evt.client.ip(), track.rtcpPort)
						delete(p.udpClientsByAddr, key)
					}
				}
				p.paths[evt.client.pathName].publisherSetNotReady()
				close(evt.done)

			case programEventClientFrameUdp:
				pub, ok := p.udpClientsByAddr[makeUdpClientAddr(evt.addr.IP, evt.addr.Port)]
				if !ok {
					continue
				}

				// client sent RTP on RTCP port or vice-versa
				if pub.streamType != evt.streamType {
					continue
				}

				pub.client.rtcpReceivers[pub.trackId].OnFrame(evt.streamType, evt.buf)
				p.forwardFrame(pub.client.pathName, pub.trackId, evt.streamType, evt.buf)

			case programEventClientFrameTcp:
				p.forwardFrame(evt.path, evt.trackId, evt.streamType, evt.buf)

			case programEventSourceReady:
				evt.source.log("ready")
				p.paths[evt.source.pathName].publisherSetReady()

			case programEventSourceNotReady:
				evt.source.log("not ready")
				p.paths[evt.source.pathName].publisherSetNotReady()

			case programEventSourceFrame:
				p.forwardFrame(evt.source.pathName, evt.trackId, evt.streamType, evt.buf)

			case programEventTerminate:
				break outer
			}
		}
	}

	go func() {
		for rawEvt := range p.events {
			switch evt := rawEvt.(type) {
			case programEventMetrics:
				evt.res <- nil

			case programEventClientClose:
				close(evt.done)

			case programEventClientDescribe:
				evt.client.describeRes <- describeRes{nil, fmt.Errorf("terminated")}

			case programEventClientAnnounce:
				evt.res <- fmt.Errorf("terminated")

			case programEventClientSetupPlay:
				evt.res <- fmt.Errorf("terminated")

			case programEventClientSetupRecord:
				evt.res <- fmt.Errorf("terminated")

			case programEventClientPlay1:
				evt.res <- fmt.Errorf("terminated")

			case programEventClientPlay2:
				close(evt.done)

			case programEventClientPlayStop:
				close(evt.done)

			case programEventClientRecord:
				close(evt.done)

			case programEventClientRecordStop:
				close(evt.done)
			}
		}
	}()

	for _, cmd := range p.cmds {
		cmd.Process.Signal(os.Interrupt)
		cmd.Wait()
	}

	for _, s := range p.sources {
		s.events <- sourceEventTerminate{}
		<-s.done
	}

	p.serverRtsp.close()
	p.serverRtcp.close()
	p.serverRtp.close()

	for c := range p.clients {
		c.conn.NetConn().Close()
		<-c.done
	}

	if p.metrics != nil {
		p.metrics.close()
	}

	if p.logFile != nil {
		p.logFile.Close()
	}

	close(p.events)
	close(p.done)
}

func (p *program) close() {
	p.events <- programEventTerminate{}
	<-p.done
}

func (p *program) findConfForPath(name string) *confPath {
	if confp, ok := p.conf.Paths[name]; ok {
		return confp
	}

	if confp, ok := p.conf.Paths["all"]; ok {
		return confp
	}

	return nil
}

func (p *program) forwardFrame(path string, trackId int, streamType gortsplib.StreamType, frame []byte) {
	for c := range p.clients {
		if c.pathName != path ||
			c.state != clientStatePlay {
			continue
		}

		track, ok := c.streamTracks[trackId]
		if !ok {
			continue
		}

		if c.streamProtocol == gortsplib.StreamProtocolUdp {
			if streamType == gortsplib.StreamTypeRtp {
				p.serverRtp.write(&udpAddrBufPair{
					addr: &net.UDPAddr{
						IP:   c.ip(),
						Zone: c.zone(),
						Port: track.rtpPort,
					},
					buf: frame,
				})
			} else {
				p.serverRtcp.write(&udpAddrBufPair{
					addr: &net.UDPAddr{
						IP:   c.ip(),
						Zone: c.zone(),
						Port: track.rtcpPort,
					},
					buf: frame,
				})
			}

		} else {
			buf := c.writeBuf.swap()
			buf = buf[:len(frame)]
			copy(buf, frame)

			c.events <- clientEventFrameTcp{
				frame: &gortsplib.InterleavedFrame{
					TrackId:    trackId,
					StreamType: streamType,
					Content:    buf,
				},
			}
		}
	}
}

func main() {
	_, err := newProgram(os.Args[1:], os.Stdin)
	if err != nil {
		log.Fatal("ERR:", err)
	}

	select {}
}
