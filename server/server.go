package server

import (
	"context"
	"crypto/tls"
	"fmt"
	stdlog "log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"

	"github.com/max-siemkin/my-custom-ingress-controller/watcher"
	"github.com/sirupsen/logrus"
	"golang.org/x/net/http2"
	"golang.org/x/sync/errgroup"
	networking "k8s.io/api/networking/v1"
)

type Server struct {
	mtx       sync.Mutex
	resources map[string]resource
	// ratelimit sync.Map
}
type resource struct {
	Config       map[string]string
	Certificates map[string]*tls.Certificate
	Backends     []backend
}
type backend struct {
	path     string
	pathType networking.PathType
	url      *url.URL
}

func New() *Server { return &Server{mtx: sync.Mutex{}, resources: map[string]resource{}} }
func (s *Server) Run(ctx context.Context) error {
	log := logrus.WithField("Source", "server.Run")

	eg, _ := errgroup.WithContext(ctx)
	eg.Go(func() error {
		srv := http.Server{
			Addr:     ":443",
			ErrorLog: stdlog.New(log.Writer(), "", 0),
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Load backend url
				s.mtx.Lock()
				resource, ok := s.resources[strings.Split(r.Host, ":")[0]]
				s.mtx.Unlock()
				if ok {
					for _, backend := range resource.Backends {
						if (backend.pathType == networking.PathTypePrefix && strings.HasPrefix(r.URL.Path, backend.path)) ||
							(backend.pathType == networking.PathTypeExact && r.URL.Path == backend.path) {
							log.WithFields(logrus.Fields{"host": r.Host, "path": r.URL.Path, "backend": backend.url.String()}).Infoln("proxying request")
							// Reverse proxy
							p := httputil.NewSingleHostReverseProxy(backend.url)
							if backend.url.Scheme == "https" {
								p.Transport = &http2.Transport{AllowHTTP: true}
							}
							p.ErrorLog = stdlog.New(log.Writer(), "", 0)
							p.ServeHTTP(w, r)
							break
						}
					}
				} else {
					http.Error(w, "service not found", http.StatusNotFound)
				}
			}),
		}
		srv.TLSConfig = &tls.Config{
			GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
				s.mtx.Lock()
				resource, ok := s.resources[strings.Split(hello.ServerName, ":")[0]]
				s.mtx.Unlock()
				if ok {
				next:
					for host, cert := range resource.Certificates {
						for strings.HasPrefix(host, "*.") {
							sn := strings.SplitN(hello.ServerName, ".", 2)
							if len(sn) <= 1 {
								continue next
							}
							host, hello.ServerName = host[2:], sn[1]
						}
						if host == hello.ServerName {
							return cert, nil
						}
					}
				}
				return nil, fmt.Errorf("certificate not found for %s", hello.ServerName)
			},
		}
		log.Infof("Starting HTTPS server(%s)\n", srv.Addr)

		if err := srv.ListenAndServeTLS("", ""); err != nil {
			return fmt.Errorf("error HTTPS server: %w", err)
		}
		return nil
	})
	eg.Go(func() error {
		srv := http.Server{
			Addr:     ":80",
			ErrorLog: stdlog.New(log.Writer(), "", 0),
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Redirect from http to https
				lnk := *r.URL
				lnk.Host, lnk.Scheme = r.Host, "https"
				http.Redirect(w, r, lnk.String(), http.StatusFound)
			}),
		}
		log.Infof("Starting HTTP server(%s)\n", srv.Addr)
		if err := srv.ListenAndServe(); err != nil {
			return fmt.Errorf("error HTTP server: %w", err)
		}
		return nil
	})
	return eg.Wait()
}
func (s *Server) Update(payload *watcher.Payload) {
	log := logrus.WithField("Source", "server.Update")

	for _, ingressPayload := range payload.Ingresses {
		for _, rule := range ingressPayload.Ingress.Spec.Rules {
			scheme, r := "http", resource{Config: map[string]string{}, Certificates: map[string]*tls.Certificate{}}
			// Init ingress annotations
			for k, v := range ingressPayload.Ingress.Annotations {
				r.Config[k] = v
			}
			// Init certs
			for _, t := range ingressPayload.Ingress.Spec.TLS {
				for _, host := range t.Hosts {
					if cert, ok := payload.TLSCertificates[t.SecretName]; ok {
						r.Certificates[host] = cert
					}
				}
			}
			// Init backends
			for _, path := range rule.HTTP.Paths {
				lnk := fmt.Sprintf("%s://%s:%d", scheme, path.Backend.Service.Name, path.Backend.Service.Port.Number)
				link, err := url.Parse(lnk)
				if err != nil {
					log.WithError(err).Errorf("url.Parse(%s)", lnk)
					continue
				}
				r.Backends = append(r.Backends, backend{path: path.Path, pathType: *path.PathType, url: link})
			}
			// Save
			s.mtx.Lock()
			s.resources[rule.Host] = r
			s.mtx.Unlock()
		}
	}
}
