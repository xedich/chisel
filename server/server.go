package chserver

import (
	"crypto/tls"
	"errors"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"strconv"

	socks5 "github.com/armon/go-socks5"
	"github.com/gorilla/websocket"
	chshare "github.com/jpillora/chisel/share"
	"github.com/jpillora/requestlog"
	"golang.org/x/crypto/ssh"
)

// Config is the configuration for the chisel service
type Config struct {
	KeySeed  string
	AuthFile string
	Auth     string
	Proxy    string
	Socks5   bool
	Reverse  bool
}

// Server respresent a chisel service
type Server struct {
	*chshare.Logger
	connStats    chshare.ConnStats
	fingerprint  string
	httpServer   *chshare.HTTPServer
	reverseProxy *httputil.ReverseProxy
	sessCount    int32
	sessions     *chshare.Users
	socksServer  *socks5.Server
	sshConfig    *ssh.ServerConfig
	users        *chshare.UserIndex
	reverseOk    bool
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

// NewServer creates and returns a new chisel server
func NewServer(config *Config) (*Server, error) {
	s := &Server{
		httpServer: chshare.NewHTTPServer(),
		Logger:     chshare.NewLogger("server"),
		sessions:   chshare.NewUsers(),
		reverseOk:  config.Reverse,
	}
	s.Info = true
	s.users = chshare.NewUserIndex(s.Logger)
	if config.AuthFile != "" {
		if err := s.users.LoadUsers(config.AuthFile); err != nil {
			return nil, err
		}
	}
	if config.Auth != "" {
		u := &chshare.User{Addrs: []*regexp.Regexp{chshare.UserAllowAll}}
		u.Name, u.Pass = chshare.ParseAuth(config.Auth)
		if u.Name != "" {
			s.users.AddUser(u)
		}
	}
	//generate private key (optionally using seed)
	key, _ := chshare.GenerateKey(config.KeySeed)
	//convert into ssh.PrivateKey
	private, err := ssh.ParsePrivateKey(key)
	if err != nil {
		log.Fatal("Failed to parse key")
	}
	//fingerprint this key
	s.fingerprint = chshare.FingerprintKey(private.PublicKey())
	//create ssh config
	s.sshConfig = &ssh.ServerConfig{
		ServerVersion:    "SSH-" + chshare.ProtocolVersion + "-server",
		PasswordCallback: s.authUser,
	}
	s.sshConfig.AddHostKey(private)
	//setup reverse proxy
	if config.Proxy != "" {
		u, err := url.Parse(config.Proxy)
		if err != nil {
			return nil, err
		}
		if u.Host == "" {
			return nil, s.Errorf("Missing protocol (%s)", u)
		}
		s.reverseProxy = httputil.NewSingleHostReverseProxy(u)
		s.reverseProxy.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
		oldDirector := s.reverseProxy.Director

		// revere secure host has a port = normal host port + 1
		reverseSecureHost := func() string {
			host, port, err := net.SplitHostPort(u.Host)
			if err != nil {
				return net.JoinHostPort(u.Host, "81")
			} else {
				port, _ := strconv.Atoi(port)
				return net.JoinHostPort(host, strconv.Itoa(port+1))
			}
		}()
		s.reverseProxy.Director = func(r *http.Request) {
			oldDirector(r)

			// if this is a request through CloudFlare (with flexible ssl), then we create a secure request
			// to the local endpoint
			// [browser] <==flexible ssl==> [cloudflare] <==plain==> [chisel server] <==self-signed ssl==> [local lan]
			if r.URL.Scheme == "http" && r.Header.Get("Cf-Visitor") == `{"scheme":"https"}` {
				r.URL.Host = reverseSecureHost
				r.URL.Scheme = "https"
			}
		}
	}
	//setup socks server (not listening on any port!)
	if config.Socks5 {
		socksConfig := &socks5.Config{}
		if s.Debug {
			socksConfig.Logger = log.New(os.Stdout, "[socks]", log.Ldate|log.Ltime)
		} else {
			socksConfig.Logger = log.New(ioutil.Discard, "", 0)
		}
		s.socksServer, err = socks5.New(socksConfig)
		if err != nil {
			return nil, err
		}
		s.Infof("SOCKS5 server enabled")
	}
	//print when reverse tunnelling is enabled
	if config.Reverse {
		s.Infof("Reverse tunnelling enabled")
	}
	return s, nil
}

// Run is responsible for starting the chisel service
func (s *Server) Run(host, port string) error {
	if err := s.Start(host, port); err != nil {
		return err
	}

	return s.Wait()
}

// Start is responsible for kicking off the http server
func (s *Server) Start(host, port string) error {
	s.Infof("Fingerprint %s", s.fingerprint)
	if s.users.Len() > 0 {
		s.Infof("User authenication enabled")
	}
	if s.reverseProxy != nil {
		s.Infof("Reverse proxy enabled")
	}
	s.Infof("Listening on %s:%s...", host, port)
	h := http.Handler(http.HandlerFunc(s.handleClientHandler))
	if s.Debug {
		h = requestlog.Wrap(h)
	}
	return s.httpServer.GoListenAndServe(host+":"+port, h)
}

// Wait waits for the http server to close
func (s *Server) Wait() error {
	return s.httpServer.Wait()
}

// Close forcibly closes the http server
func (s *Server) Close() error {
	return s.httpServer.Close()
}

// GetFingerprint is used to access the server fingerprint
func (s *Server) GetFingerprint() string {
	return s.fingerprint
}

// authUser is responsible for validating the ssh user / password combination
func (s *Server) authUser(c ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
	// check if user authenication is enable and it not allow all
	if s.users.Len() == 0 {
		return nil, nil
	}
	// check the user exists and has matching password
	n := c.User()
	user, found := s.users.Get(n)
	if !found || user.Pass != string(password) {
		s.Debugf("Login failed for user: %s", n)
		return nil, errors.New("Invalid authentication for username: %s")
	}
	// insert the user session map
	// @note: this should probably have a lock on it given the map isn't thread-safe??
	s.sessions.Set(string(c.SessionID()), user)
	return nil, nil
}

// AddUser adds a new user into the server user index
func (s *Server) AddUser(user, pass string, addrs ...string) error {
	authorizedAddrs := make([]*regexp.Regexp, 0)

	for _, addr := range addrs {
		authorizedAddr, err := regexp.Compile(addr)
		if err != nil {
			return err
		}

		authorizedAddrs = append(authorizedAddrs, authorizedAddr)
	}

	u := &chshare.User{Name: user, Pass: pass, Addrs: authorizedAddrs}
	s.users.AddUser(u)
	return nil
}

// DeleteUser removes a user from the server user index
func (s *Server) DeleteUser(user string) {
	s.users.Del(user)
}
