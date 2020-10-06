package cloudgame

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"time"

	"github.com/giongto35/cloud-morph/pkg/addon/textchat"

	"github.com/giongto35/cloud-morph/pkg/common/ws"
	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v2"

	"gopkg.in/yaml.v2"

	"github.com/pion/rtp"
)

const (
	// CollaborativeMode Multiple users share the same app session
	CollaborativeMode = "collaborative"
	// OnDemandMode Multiple users runs on a new available machine
	OnDemandMode = "ondemand"
)

var webrtcconfig = webrtc.Configuration{ICEServers: []webrtc.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302"}}}}
var isStarted bool

type Service struct {
	clients          map[string]*Client
	gameEvents       chan WSPacket
	chatEvents       chan textchat.ChatMessage
	appModeHandler   *appModeHandler
	discoveryHandler *discoveryHandler
	ccApp            CloudGameClient
	config           Config
	chat             *textchat.TextChat
}

type Client struct {
	clientID     string
	conn         *websocket.Conn
	rtcConn      *webrtc.PeerConnection
	chatEvents   chan textchat.ChatMessage
	videoStream  chan rtp.Packet
	videoTrack   *webrtc.Track
	serverEvents chan WSPacket
	done         chan struct{}
	// TODO: Get rid of ssrc
	ssrc uint32
}

type AppHost struct {
	// Host string `json:"host"`
	Addr    string `json:"addr"`
	AppName string `json:"app_name"`
}

type Config struct {
	Path    string `yaml:"path"`
	AppFile string `yaml:"appFile"`
	// To help WinAPI search the app
	WindowTitle string `yaml:"windowTitle"`
	HWKey       bool   `yaml:"hardwareKey"`
	AppMode     string `yaml:"appMode"`
	AppName     string `yaml:"appName"`
	// Discovery service
	DiscoveryHost string `yaml:"discoveryHost"`
}

type instance struct {
	addr string
}

type appModeHandler struct {
	appMode            string
	availableInstances []instance
}

type discoveryHandler struct {
	httpClient    *http.Client
	discoveryHost string
}

func NewAppMode(appMode string) *appModeHandler {
	return &appModeHandler{
		appMode: appMode,
	}
}

// Heartbeat maintains connection to server
func (c *Client) Heartbeat() {
	// send heartbeat every 1s
	timer := time.Tick(time.Second)

	for range timer {
		select {
		case <-c.done:
			log.Println("Close heartbeat")
			return
		default:
		}
		// c.Send({PType: "heartbeat"})
	}
}

func (c *Client) Listen() {
	defer func() {
		close(c.done)
	}()

	// Listen from video stream
	go func() {
		for packet := range c.videoStream {
			if c.videoTrack == nil {
				continue
			}
			if writeErr := c.videoTrack.WriteRTP(&packet); writeErr != nil {
				panic(writeErr)
			}
		}
	}()

	log.Println("Client listening")
	for {
		_, rawMsg, err := c.conn.ReadMessage()
		fmt.Println("received", rawMsg)
		if err != nil {
			log.Println("[!] read:", err)
			// TODO: Check explicit disconnect error to break
			break
		}
		wspacket := ws.Packet{}
		err = json.Unmarshal(rawMsg, &wspacket)

		// TODO: Refactor
		if wspacket.PType == "OFFER" {
			c.signal(wspacket.Data)
			// c.Send(cloudgame.WSPacket{
			// 	PType: "Answer
			// })
			continue
		}
		if err != nil {
			log.Println("error decoding", err)
			continue
		}
		if wspacket.PType == "CHAT" {
			c.chatEvents <- textchat.Convert(wspacket)
		} else {
			c.serverEvents <- Convert(wspacket)
		}
	}

}

func (c *Client) Send(packet WSPacket) {
	data, err := json.Marshal(packet)
	if err != nil {
		return
	}

	c.conn.WriteMessage(websocket.TextMessage, data)
}

func (c *Client) signal(offerString string) {
	log.Println("Signalling")
	RTCConn, err := webrtc.NewPeerConnection(webrtcconfig)
	if err != nil {
		log.Println("error ", err)
	}
	c.rtcConn = RTCConn

	offer := webrtc.SessionDescription{}
	Decode(offerString, &offer)

	err = RTCConn.SetRemoteDescription(offer)
	if err != nil {
		log.Println("Set remote description from peer failed", err)
		return
	}

	log.Println("Get SSRC", c.ssrc)
	videoTrack := streamRTP(RTCConn, offer, c.ssrc)

	var answer webrtc.SessionDescription
	answer, err = RTCConn.CreateAnswer(nil)
	if err != nil {
		log.Println("Create Answer Failed", err)
		return
	}

	err = RTCConn.SetLocalDescription(answer)
	if err != nil {
		log.Println("Set Local Description Failed", err)
		return
	}

	isStarted = true
	log.Println("Sending answer", answer)
	c.Send(WSPacket{
		PType: "ANSWER",
		Data:  Encode(answer),
	})
	c.videoTrack = videoTrack
}
func (d *discoveryHandler) GetAppHosts() []AppHost {
	type GetAppHostsResponse struct {
		AppHosts []AppHost `json:"apps"`
	}
	var resp GetAppHostsResponse

	rawResp, err := d.httpClient.Get(d.discoveryHost + "/get-apps")
	if err != nil {
		// log.Warn(err)
		fmt.Println(err)
	}

	defer rawResp.Body.Close()
	json.NewDecoder(rawResp.Body).Decode(&resp)

	return resp.AppHosts
}

func (d *discoveryHandler) Register(addr string, appName string) error {
	type RegisterAppRequest struct {
		Addr    string `json:"addr"`
		AppName string `json:"app_name"`
	}
	req := RegisterAppRequest{
		Addr:    addr,
		AppName: appName,
	}

	reqBytes, err := json.Marshal(req)
	if err != nil {
		return nil
	}

	_, err = d.httpClient.Post(d.discoveryHost+"/register", "application/json", bytes.NewBuffer(reqBytes))
	if err != nil {
		return nil
	}

	return err
}

func NewDiscovery(discoveryHost string) *discoveryHandler {
	return &discoveryHandler{
		httpClient: &http.Client{
			Timeout: time.Second * 10,
		},
		discoveryHost: discoveryHost,
	}
}

func (s *Service) GetAppHosts() []AppHost {
	return s.discoveryHandler.GetAppHosts()
}

func (s *Service) Register(addr string) error {
	return s.discoveryHandler.Register(addr, s.config.AppName)
}

func readConfig(path string) (Config, error) {
	cfgyml, err := ioutil.ReadFile(path)
	if err != nil {
		return Config{}, err
	}

	cfg := Config{}
	err = yaml.Unmarshal(cfgyml, &cfg)

	if cfg.AppName == "" {
		cfg.AppName = cfg.WindowTitle
	}
	return cfg, err
}

func NewServer() *Server {
	server := &Server{
		clients:    map[string]*Client{},
		gameEvents: make(chan WSPacket, 1),
		chatEvents: make(chan textchat.ChatMessage, 1),
	}

	return server
}

// func NewCloudGameClient(cfg Config, gameEvents chan WSPacket) *ccImpl {
func NewCloudService(configFilePath string, gameEvents chan WSPacket) *Service {
	cfg, err := readConfig(configFilePath)
	if err != nil {
		panic(err)
	}

	return &Service{
		server:           NewServer(),
		appModeHandler:   NewAppMode(cfg.AppMode),
		discoveryHandler: NewDiscovery(cfg.DiscoveryHost),
		ccApp:            NewCloudGameClient(cfg, gameEvents),
		config:           cfg,
	}
}

func (s *Service) VideoStream() chan rtp.Packet {
	return s.ccApp.VideoStream()
}

func (s *Service) SendInput(packet WSPacket) {
	s.ccApp.SendInput(packet)
}

func (s *Service) GetSSRC() uint32 {
	return s.ccApp.GetSSRC()
}

func (s *Service) Handle() {
	s.ccApp.Handle()
}

// Encode encodes the input in base64
// It can optionally zip the input before encoding
func Encode(obj interface{}) string {
	b, err := json.Marshal(obj)
	if err != nil {
		panic(err)
	}

	return base64.StdEncoding.EncodeToString(b)
}

// Decode decodes the input from base64
// It can optionally unzip the input after decoding
func Decode(in string, obj interface{}) {
	b, err := base64.StdEncoding.DecodeString(in)
	if err != nil {
		panic(err)
	}

	err = json.Unmarshal(b, obj)
	if err != nil {
		panic(err)
	}
}

// streapRTP is based on to https://github.com/pion/webrtc/tree/master/examples/rtp-to-webrtc
// It fetches from a RTP stream produced by FFMPEG and broadcast to all webRTC sessions
func streamRTP(conn *webrtc.PeerConnection, offer webrtc.SessionDescription, ssrc uint32) *webrtc.Track {
	// We make our own mediaEngine so we can place the sender's codecs in it.  This because we must use the
	// dynamic media type from the sender in our answer. This is not required if we are the offerer
	mediaEngine := webrtc.MediaEngine{}
	err := mediaEngine.PopulateFromSDP(offer)
	if err != nil {
		panic(err)
	}

	// Create a video track, using the same SSRC as the incoming RTP Pack)
	videoTrack, err := conn.NewTrack(webrtc.DefaultPayloadTypeVP8, ssrc, "video", "pion")
	if err != nil {
		panic(err)
	}
	if _, err = conn.AddTrack(videoTrack); err != nil {
		panic(err)
	}
	log.Println("video track", videoTrack)

	// Set the handler for ICE connection state
	// This will notify you when the peer has connected/disconnected
	conn.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		log.Printf("Connection State has changed %s \n", connectionState.String())
	})

	// Set the remote SessionDescription
	if err = conn.SetRemoteDescription(offer); err != nil {
		panic(err)
	}
	log.Println("Done creating videotrack")

	return videoTrack
}

func (o *Server) Handle() {
	// Spawn CloudGaming Handle
	go o.cgame.Handle()
	// Spawn Chat Handle
	go o.chat.Handle()

	// Fanout output channel
	go func() {
		for p := range o.cgame.VideoStream() {
			for _, client := range o.clients {
				client.videoStream <- p
			}
		}
	}()
}
