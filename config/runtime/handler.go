package runtime

import (
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/sirupsen/logrus"

	ac "go.avenga.cloud/couper/gateway/access_control"
	"go.avenga.cloud/couper/gateway/config"
	"go.avenga.cloud/couper/gateway/errors"
	"go.avenga.cloud/couper/gateway/handler"
	"go.avenga.cloud/couper/gateway/internal/seetie"
	"go.avenga.cloud/couper/gateway/utils"
)

type entrypointHandler struct {
	api        map[*config.Endpoint]http.Handler
	files, spa http.Handler
}

const (
	backendDefaultConnectTimeout = "10s"
	backendDefaultTimeout        = "300s"
	backendDefaultTTFBTimeout    = "60s"
)

var errorMissingBackend = fmt.Errorf("no backend attribute reference or block")

// BuildEntrypointHandlers sets http handler specific defaults and validates the given gateway configuration.
// Wire up all endpoints and maps them within the returned EntrypointHandlers.
func BuildEntrypointHandlers(conf *config.Gateway, httpConf *HTTPConfig, log *logrus.Entry) EntrypointHandlers {
	if len(conf.Server) == 0 {
		log.Fatal("Missing server definitions")
	}

	type backendDefinition struct {
		conf    *config.Backend
		handler http.Handler
	}

	backends := make(map[string]backendDefinition)
	entryHandler := &entrypointHandler{api: make(map[*config.Endpoint]http.Handler)}

	if conf.Definitions != nil {
		for _, beConf := range conf.Definitions.Backend {
			if _, ok := backends[beConf.Name]; ok {
				log.Fatalf("backend name must be unique: '%s'", beConf.Name)
			}

			if beConf.Origin == "" {
				log.Fatalf("backend %q: origin attribute is required", beConf.Name)
			}

			if beConf.Timeout == "" {
				beConf.Timeout = backendDefaultTimeout
			}
			if beConf.TTFBTimeout == "" {
				beConf.TTFBTimeout = backendDefaultTTFBTimeout
			}
			if beConf.ConnectTimeout == "" {
				beConf.ConnectTimeout = backendDefaultConnectTimeout
			}
			t, ttfbt, ct := parseBackendTimings(beConf)
			proxy, err := handler.NewProxy(&handler.ProxyOptions{
				ConnectTimeout: ct,
				Context:        []hcl.Body{beConf.Options},
				Hostname:       beConf.Hostname,
				Origin:         beConf.Origin,
				Path:           beConf.Path,
				Timeout:        t,
				TTFBTimeout:    ttfbt,
			}, log, conf.Context)
			if err != nil {
				log.Fatal(err)
			}
			backends[beConf.Name] = backendDefinition{
				conf:    beConf,
				handler: proxy,
			}
		}
	}

	accessControls := configureAccessControls(conf)

	handlers := make(EntrypointHandlers, 0)

	for idx, server := range conf.Server {
		configureBasePathes(server)

		mux := &Mux{
			APIErrTpl: errors.DefaultJSON,
			FSErrTpl:  errors.DefaultHTML,
		}

		wd, err := os.Getwd()
		if err != nil {
			log.Fatal(err)
		}

		if server.Files != nil {
			if server.Files.ErrorFile != "" {
				if mux.FSErrTpl, err = errors.NewTemplateFromFile(path.Join(wd, server.Files.ErrorFile)); err != nil {
					log.Fatal(err)
				}
			}
			entryHandler.files = handler.NewFile(wd, server.Files.BasePath, server.Files.DocumentRoot, mux.FSErrTpl)
			entryHandler.files = configureProtectedHandler(accessControls, mux.FSErrTpl,
				config.NewAccessControl(server.AccessControl, server.DisableAccessControl),
				config.NewAccessControl(server.Files.AccessControl, server.Files.DisableAccessControl), entryHandler.files)
		}

		if server.Spa != nil {
			entryHandler.spa = handler.NewSpa(wd, server.Spa.BootstrapFile)
			entryHandler.spa = configureProtectedHandler(accessControls, errors.DefaultHTML,
				config.NewAccessControl(server.AccessControl, server.DisableAccessControl),
				config.NewAccessControl(server.Spa.AccessControl, server.Spa.DisableAccessControl), entryHandler.spa)
		}

		if server.Files != nil {
			mux.FS = make(Routes, 0)
			mux.FSPath = server.Files.BasePath
			mux.FS = mux.FS.Add(
				utils.JoinPath(server.Files.BasePath, "/**"),
				entryHandler.files,
			)

			// Register base_path-302 case
			if server.Files.BasePath != "/" {
				mux.FS = mux.FS.Add(
					strings.TrimRight(server.Files.BasePath, "/")+"$",
					entryHandler.files,
				)
			}
		}

		if server.Spa != nil {
			mux.SPA = make(Routes, 0)
			mux.SPAPath = server.Spa.BasePath

			for _, spaPath := range server.Spa.Paths {
				spaPath := utils.JoinPath(server.Spa.BasePath, spaPath)

				mux.SPA = mux.SPA.Add(
					spaPath,
					entryHandler.spa,
				)

				if spaPath != "/**" && strings.HasSuffix(spaPath, "/**") {
					mux.SPA = mux.SPA.Add(
						spaPath[:len(spaPath)-len("/**")],
						entryHandler.spa,
					)
				}
			}
		}

		if server.API == nil {
			if err = configureHandlers(httpConf, server, mux, handlers); err != nil {
				log.Fatal(err)
			}
			continue
		}

		mux.API = make(Routes, 0)
		mux.APIPath = server.API.BasePath

		if server.API.ErrorFile != "" {
			if mux.APIErrTpl, err = errors.NewTemplateFromFile(path.Join(wd, server.API.ErrorFile)); err != nil {
				log.Fatal(err)
			}
		}

		// map backends to endpoint
		endpoints := make(map[string]bool)
		for e, endpoint := range server.API.Endpoint {
			conf.Server[idx].API.Endpoint[e].Server = server // assign parent
			if endpoints[endpoint.Pattern] {
				log.Fatal("Duplicate endpoint: ", endpoint.Pattern)
			}
			endpoints[endpoint.Pattern] = true

			// setACHandlerFn individual wrap for access_control configuration per endpoint
			setACHandlerFn := func(protectedBackend backendDefinition) {
				protectedHandler := protectedBackend.handler

				// prefer endpoint 'path' definition over 'backend.Path'
				if endpoint.Path != "" {
					beConf, remainCtx := protectedBackend.conf.Merge(&config.Backend{Path: endpoint.Path})
					t, ttfbt, ct := parseBackendTimings(beConf)
					proxy, err := handler.NewProxy(&handler.ProxyOptions{
						ConnectTimeout: ct,
						Context:        remainCtx,
						Hostname:       beConf.Hostname,
						Origin:         beConf.Origin,
						Path:           beConf.Path,
						Timeout:        t,
						TTFBTimeout:    ttfbt,
					}, log, conf.Context)
					if err != nil {
						log.Fatal(err)
					}
					protectedHandler = proxy
				}

				entryHandler.api[endpoint] = configureProtectedHandler(accessControls, mux.APIErrTpl,
					config.NewAccessControl(server.AccessControl, server.DisableAccessControl).
						Merge(config.NewAccessControl(server.API.AccessControl, server.API.DisableAccessControl)),
					config.NewAccessControl(endpoint.AccessControl, endpoint.DisableAccessControl),
					protectedHandler)
			}

			// lookup for backend reference, prefer endpoint definition over api one
			if endpoint.Backend != "" {
				if _, ok := backends[endpoint.Backend]; !ok {
					log.Fatalf("backend %q is not defined", endpoint.Backend)
				}
				setACHandlerFn(backends[endpoint.Backend])
				continue
			}

			// otherwise try to parse an inline block and fallback for api reference or inline block
			inlineBackend, inlineConf, err := newInlineBackend(conf.Context, endpoint.InlineDefinition, log)
			if err == errorMissingBackend {
				if server.API.Backend != "" {
					if _, ok := backends[server.API.Backend]; !ok {
						log.Fatalf("backend %q is not defined", server.API.Backend)
					}
					setACHandlerFn(backends[server.API.Backend])
					continue
				}
				inlineBackend, inlineConf, err = newInlineBackend(conf.Context, server.API.InlineDefinition, log)
				if err != nil {
					log.Fatal(err)
				}

				if inlineConf.Name == "" && inlineConf.Origin == "" {
					log.Fatal("api inline backend requires an origin attribute")
				}

			} else if err != nil {
				if err == handler.OriginRequiredError && inlineConf.Name == "" || err != handler.OriginRequiredError {
					log.Fatalf("Range: %s: %v", endpoint.InlineDefinition.MissingItemRange().String(), err) // TODO diags error
				}
			}

			if inlineConf.Name != "" { // inline backends have no label, assume a reference and override settings
				if _, ok := backends[inlineConf.Name]; !ok {
					log.Fatalf("override backend %q is not defined", inlineConf.Name)
				}

				beConf, remainCtx := backends[inlineConf.Name].conf.Merge(inlineConf)
				t, ttfbt, ct := parseBackendTimings(beConf)
				proxy, err := handler.NewProxy(&handler.ProxyOptions{
					ConnectTimeout: ct,
					Context:        remainCtx,
					Hostname:       beConf.Hostname,
					Origin:         beConf.Origin,
					Path:           beConf.Path,
					Timeout:        t,
					TTFBTimeout:    ttfbt,
				}, log, conf.Context)
				if err != nil {
					log.Fatal(err)
				}
				inlineBackend = proxy
			}

			setACHandlerFn(backendDefinition{conf: inlineConf, handler: inlineBackend})
		}

		for _, endpoint := range server.API.Endpoint {
			mux.API = mux.API.Add(
				utils.JoinPath(server.API.BasePath, endpoint.Pattern),
				entryHandler.api[endpoint],
			)
		}

		if err = configureHandlers(httpConf, server, mux, handlers); err != nil {
			log.Fatal(err)
		}
	}
	return handlers
}

func configureBasePathes(server *config.Server) {
	if server.BasePath == "" {
		server.BasePath = "/"
	}
	if !strings.HasSuffix(server.BasePath, "/") {
		server.BasePath = server.BasePath + "/"
	}
	if server.Files != nil {
		server.Files.BasePath = path.Join(server.BasePath, server.Files.BasePath)
		if !strings.HasSuffix(server.Files.BasePath, "/") {
			server.Files.BasePath = server.Files.BasePath + "/"
		}
	}
	if server.Spa != nil {
		server.Spa.BasePath = path.Join(server.BasePath, server.Spa.BasePath) + "/"
		if !strings.HasSuffix(server.Spa.BasePath, "/") {
			server.Spa.BasePath = server.Spa.BasePath + "/"
		}
	}
	if server.API != nil {
		server.API.BasePath = path.Join(server.BasePath, server.API.BasePath) + "/"
		if !strings.HasSuffix(server.API.BasePath, "/") {
			server.API.BasePath = server.API.BasePath + "/"
		}
	}
}

func configureHandlers(conf *HTTPConfig, server *config.Server, mux *Mux, handlers EntrypointHandlers) error {
	hosts := server.Hosts
	if len(hosts) == 0 {
		hosts = []string{fmt.Sprintf("*:%d", conf.ListenPort)}
	}

	for _, hp := range hosts {
		var host string
		port := Port(strconv.Itoa(conf.ListenPort))

		if strings.IndexByte(hp, ':') == -1 {
			host = hp
		} else {
			h, p, err := net.SplitHostPort(hp)
			if err != nil {
				return err
			} else if p != "" {
				port = Port(p)
			}
			host = h
		}

		if _, ok := handlers[port]; !ok {
			handlers[port] = make(HostHandlers, 0)
		}

		if _, ok := handlers[port][host]; ok {
			return fmt.Errorf("multiple <host:port> combination found: '%s:%s'", host, port)
		}

		handlers[port][host] = &ServerMux{Server: server, Mux: mux}
	}
	return nil
}

func configureAccessControls(conf *config.Gateway) ac.Map {
	accessControls := make(ac.Map)
	if conf.Definitions != nil {
		for _, jwt := range conf.Definitions.JWT {
			var jwtSource ac.Source
			var jwtKey string
			if jwt.Cookie != "" {
				jwtSource = ac.Cookie
				jwtKey = jwt.Cookie
			} else if jwt.Header != "" {
				jwtSource = ac.Header
				jwtKey = jwt.Header
			}
			var key []byte
			if jwt.KeyFile != "" {
				wd, _ := os.Getwd()
				content, err := ioutil.ReadFile(path.Join(wd, jwt.KeyFile))
				if err != nil {
					panic(err)
				}
				key = content
			} else if jwt.Key != "" {
				key = []byte(jwt.Key)
			}

			var claims ac.Claims
			if jwt.Claims != nil {
				c, diags := seetie.ExpToMap(conf.Context, jwt.Claims)
				if diags.HasErrors() {
					panic(diags.Error())
				}
				claims = c
			}
			j, err := ac.NewJWT(jwt.SignatureAlgorithm, jwt.Name, claims, jwt.ClaimsRequired, jwtSource, jwtKey, key)
			if err != nil {
				panic(fmt.Sprintf("loading jwt %q definition failed: %s", jwt.Name, err))
			}
			accessControls[jwt.Name] = j
		}
	}
	return accessControls
}

func configureProtectedHandler(m ac.Map, errTpl *errors.Template, parentAC, handlerAC config.AccessControl, h http.Handler) http.Handler {
	var acList ac.List
	for _, acName := range parentAC.
		Merge(handlerAC).List() {
		m.MustExist(acName)
		acList = append(acList, m[acName])
	}
	if len(acList) > 0 {
		return handler.NewAccessControl(h, errTpl, acList...)
	}
	return h
}

func newInlineBackend(evalCtx *hcl.EvalContext, inlineDef hcl.Body, log *logrus.Entry) (http.Handler, *config.Backend, error) {
	content, _, diags := inlineDef.PartialContent(config.Definitions{}.Schema(true))
	// ignore diag errors here, would fail anyway with our retry
	if content == nil || len(content.Blocks) == 0 {
		// no inline conf, retry for override definitions with label
		content, _, diags = inlineDef.PartialContent(config.Definitions{}.Schema(false))
		if diags.HasErrors() {
			return nil, nil, diags
		}

		if content == nil || len(content.Blocks) == 0 {
			return nil, nil, errorMissingBackend
		}
	}

	beConf := &config.Backend{}
	diags = gohcl.DecodeBody(content.Blocks[0].Body, evalCtx, beConf)
	if diags.HasErrors() {
		return nil, nil, diags
	}
	if len(content.Blocks[0].Labels) > 0 {
		beConf.Name = content.Blocks[0].Labels[0]
	}

	t, ttfbt, ct := parseBackendTimings(beConf)
	proxy, err := handler.NewProxy(&handler.ProxyOptions{
		ConnectTimeout: ct,
		Context:        []hcl.Body{beConf.Options},
		Hostname:       beConf.Hostname,
		Origin:         beConf.Origin,
		Path:           beConf.Path,
		Timeout:        t,
		TTFBTimeout:    ttfbt,
	}, log, evalCtx)
	return proxy, beConf, err
}

func parseBackendTimings(conf *config.Backend) (time.Duration, time.Duration, time.Duration) {
	t := conf.Timeout
	ttfb := conf.TTFBTimeout
	c := conf.ConnectTimeout
	if t == "" {
		t = backendDefaultTimeout
	}
	if ttfb == "" {
		ttfb = backendDefaultTTFBTimeout
	}
	if c == "" {
		c = backendDefaultConnectTimeout
	}
	totalD, err := time.ParseDuration(t)
	if err != nil {
		panic(err)
	}
	ttfbD, err := time.ParseDuration(ttfb)
	if err != nil {
		panic(err)
	}
	connectD, err := time.ParseDuration(c)
	if err != nil {
		panic(err)
	}
	return totalD, ttfbD, connectD
}