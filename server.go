package pcopy

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"golang.org/x/time/rate"
	"io"
	"io/fs"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/template"
	"time"
)

const (
	managerTickerInterval         = 30 * time.Second
	defaultMaxAuthAge             = time.Minute
	noAuthRequestAge              = 0
	visitorCountLimit             = 10
	visitorRequestsPerSecond      = 2
	visitorRequestsPerSecondBurst = 5
	visitorExpungeAfter           = 3 * time.Minute
	certCommonName                = "pcopy"

	headerStream       = "X-Stream"
	headerFormat       = "X-Format"
	headerFileMode     = "X-Mode"
	headerTTL          = "X-TTL"
	headerURL          = "X-Url"
	headerExpires      = "X-Expires"
	queryParamAuth     = "a"
	queryParamStream   = "s"
	queryParamFormat   = "f"
	queryParamFileMode = "m"
	queryParamTTL      = "t"
)

var (
	authHmacFormat      = "HMAC %d %d %s" // timestamp ttl b64-hmac
	authHmacRegex       = regexp.MustCompile(`^HMAC (\d+) (\d+) (.+)$`)
	authBasicRegex      = regexp.MustCompile(`^Basic (\S+)$`)
	clipboardPathFormat = "/%s"
	reservedFiles       = []string{"help", "version", "info", "verify", "static", "robots.txt", "favicon.ico"}

	//go:embed "web/index.gohtml"
	webTemplateSource string
	webTemplate       = template.Must(template.New("index").Funcs(templateFnMap).Parse(webTemplateSource))

	//go:embed "web/curl.tmpl"
	curlTemplateSource string
	curlTemplate       = template.Must(template.New("curl").Funcs(templateFnMap).Parse(curlTemplateSource))

	//go:embed web/static
	webStaticFs embed.FS
)

// server is the main HTTP server struct. It's the one with all the good stuff.
type server struct {
	config       *Config
	countLimiter *limiter
	sizeLimiter  *limiter
	visitors     map[string]*visitor
	routes       []route
	sync.Mutex
}

// visitor represents an API user, and its associated rate.Limiter used for rate limiting
type visitor struct {
	countLimiter *limiter
	rateLimiter  *rate.Limiter
	lastSeen     time.Time
}

// httpResponseInfo is the response returned by the /info endpoint
type httpResponseInfo struct {
	ServerAddr string `json:"serverAddr"`
	Salt       string `json:"salt"`
}

// httpResponsePut is the response returned when uploading a file
type httpResponsePut struct {
	URL     string `json:"url"`
	Expires int64  `json:"expires"`
}

// metaFile defines the metadata file format stored next to each file
type metaFile struct {
	Mode    string `json:"mode"`
	Expires int64  `json:"expires"`
}

// handlerFnWithErr extends the normal http.HandlerFunc to be able to easily return errors
type handlerFnWithErr func(http.ResponseWriter, *http.Request) error

// route represents a HTTP route (e.g. GET /info), a regex that matches it and its handler
type route struct {
	method  string
	regex   *regexp.Regexp
	handler handlerFnWithErr
}

func newRoute(method, pattern string, handler handlerFnWithErr) route {
	return route{method, regexp.MustCompile("^" + pattern + "$"), handler}
}

// routeCtx is a marker struct used to find fields in route matches
type routeCtx struct{}

// webTemplateConfig is a struct defining all the things required to render the web root
type webTemplateConfig struct {
	KeyDerivIter     int
	KeyLenBytes      int
	CurlPinnedPubKey string
	DefaultPort      int
	Config           *Config
}

// Serve starts a server and listens for incoming HTTPS requests. The server handles all management operations (info,
// verify, ...), as well as the actual clipboard functionality (GET/PUT/POST). It also starts a background process
// to prune old
func Serve(config *Config) error {
	server, err := newServer(config)
	if err != nil {
		return err
	}
	go server.serverManager()
	return server.listenAndServe()
}

func newServer(config *Config) (*server, error) {
	if config.ListenHTTPS == "" && config.ListenHTTP == "" {
		return nil, errListenAddrMissing
	}
	if config.ListenHTTPS != "" {
		if config.KeyFile == "" {
			return nil, errKeyFileMissing
		}
		if config.CertFile == "" {
			return nil, errCertFileMissing
		}
	}
	if err := os.MkdirAll(config.ClipboardDir, 0700); err != nil {
		return nil, errClipboardDirNotWritable
	}
	if unix.Access(config.ClipboardDir, unix.W_OK) != nil {
		return nil, errClipboardDirNotWritable
	}
	return &server{
		config:       config,
		sizeLimiter:  newLimiter(config.ClipboardSizeLimit),
		countLimiter: newLimiter(int64(config.ClipboardCountLimit)),
		visitors:     make(map[string]*visitor),
		routes:       nil,
	}, nil
}

func (s *server) listenAndServe() error {
	listens := make([]string, 0)
	if s.config.ListenHTTP != "" {
		listens = append(listens, fmt.Sprintf("%s/http", s.config.ListenHTTP))
	}
	if s.config.ListenHTTPS != "" {
		listens = append(listens, fmt.Sprintf("%s/https", s.config.ListenHTTPS))
	}
	if s.config.Key == nil {
		log.Printf("Listening on %s (UNPROTECTED CLIPBOARD)\n", strings.Join(listens, " "))
	} else {
		log.Printf("Listening on %s\n", strings.Join(listens, " "))
	}

	http.HandleFunc("/", s.handle)

	errChan := make(chan error)
	if s.config.ListenHTTP != "" {
		go func() {
			if err := http.ListenAndServe(s.config.ListenHTTP, nil); err != nil {
				errChan <- err
			}
		}()
	}
	if s.config.ListenHTTPS != "" {
		go func() {
			if err := http.ListenAndServeTLS(s.config.ListenHTTPS, s.config.CertFile, s.config.KeyFile, nil); err != nil {
				errChan <- err
			}
		}()
	}
	err := <-errChan
	return err
}

func (s *server) routeList() []route {
	if s.routes != nil {
		return s.routes
	}
	s.Lock()
	defer s.Unlock()

	s.routes = []route{
		newRoute("GET", "/", s.handleRoot),
		newRoute("PUT", "/", s.limit(s.auth(s.handleClipboardPutRandom))),
		newRoute("POST", "/", s.limit(s.auth(s.handleClipboardPutRandom))),
		newRoute("GET", "/static/.+", s.onlyIfWebUI(s.handleStatic)),
		newRoute("GET", "/info", s.limit(s.handleInfo)),
		newRoute("GET", "/verify", s.limit(s.auth(s.handleVerify))),
		newRoute("GET", "/(?i)([a-z0-9][-_.a-z0-9]{1,100})", s.limit(s.auth(s.handleClipboardGet))),
		newRoute("PUT", "/(?i)([a-z0-9][-_.a-z0-9]{1,100})", s.limit(s.auth(s.handleClipboardPut))),
		newRoute("POST", "/(?i)([a-z0-9][-_.a-z0-9]{1,100})", s.limit(s.auth(s.handleClipboardPut))),
	}
	return s.routes
}

func (s *server) handle(w http.ResponseWriter, r *http.Request) {
	for _, route := range s.routeList() {
		matches := route.regex.FindStringSubmatch(r.URL.Path)
		if len(matches) > 0 && r.Method == route.method {
			log.Printf("%s - %s %s", r.RemoteAddr, r.Method, r.RequestURI)
			ctx := context.WithValue(r.Context(), routeCtx{}, matches[1:])
			if err := route.handler(w, r.WithContext(ctx)); err != nil {
				if e, ok := err.(*errHTTPNotOK); ok {
					s.fail(w, r, e.code, e)
				} else {
					s.fail(w, r, http.StatusInternalServerError, err)
				}
			}
			return
		}
	}
	if r.Method == http.MethodGet {
		s.fail(w, r, http.StatusNotFound, errNoMatchingRoute)
	} else {
		s.fail(w, r, http.StatusBadRequest, errNoMatchingRoute)
	}
}

func (s *server) handleInfo(w http.ResponseWriter, r *http.Request) error {
	log.Printf("%s - %s %s", r.RemoteAddr, r.Method, r.RequestURI)

	salt := ""
	if s.config.Key != nil {
		salt = base64.StdEncoding.EncodeToString(s.config.Key.Salt)
	}

	response := &httpResponseInfo{
		ServerAddr: s.config.ServerAddr,
		Salt:       salt,
	}

	w.WriteHeader(http.StatusOK)
	return json.NewEncoder(w).Encode(response)
}

func (s *server) handleVerify(w http.ResponseWriter, r *http.Request) error {
	log.Printf("%s - %s %s", r.RemoteAddr, r.Method, r.RequestURI)
	return nil
}

func (s *server) handleRoot(w http.ResponseWriter, r *http.Request) error {
	if strings.HasPrefix(r.Header.Get("User-Agent"), "curl/") {
		return s.handleCurlRoot(w, r)
	}
	return s.onlyIfWebUI(s.redirectHTTPS(s.handleWebRoot))(w, r)
}

func (s *server) handleWebRoot(w http.ResponseWriter, r *http.Request) error {
	var err error
	curlPinnedPubKey := ""
	if r.TLS != nil {
		curlPinnedPubKey, err = ReadCurlPinnedPublicKeyFromFile(s.config.CertFile)
		if err != nil {
			return err
		}
	}
	return webTemplate.Execute(w, &webTemplateConfig{
		KeyDerivIter:     keyDerivIter,
		KeyLenBytes:      keyLenBytes,
		CurlPinnedPubKey: curlPinnedPubKey,
		DefaultPort:      DefaultPort,
		Config:           s.config,
	})
}

func (s *server) handleCurlRoot(w http.ResponseWriter, r *http.Request) error {
	curlPinnedPubKey := ""
	if r.TLS != nil {
		var err error
		curlPinnedPubKey, err = ReadCurlPinnedPublicKeyFromFile(s.config.CertFile)
		if err != nil {
			return err
		}
	}
	return curlTemplate.Execute(w, &webTemplateConfig{
		CurlPinnedPubKey: curlPinnedPubKey,
		Config:           s.config,
	})
}

func (s *server) handleStatic(w http.ResponseWriter, r *http.Request) error {
	r.URL.Path = "/web" + r.URL.Path // This is a hack to get the embedded path
	http.FileServer(http.FS(webStaticFs)).ServeHTTP(w, r)
	return nil
}

func (s *server) handleClipboardGet(w http.ResponseWriter, r *http.Request) error {
	fields := r.Context().Value(routeCtx{}).([]string)
	file, _, err := s.getClipboardFile(fields[0])
	if err != nil {
		return ErrHTTPBadRequest
	}

	stat, err := os.Stat(file)
	if err != nil {
		return ErrHTTPNotFound
	}
	if stat.Mode()&os.ModeNamedPipe == 0 {
		w.Header().Set("Length", strconv.FormatInt(stat.Size(), 10))
	}
	f, err := os.Open(file)
	if err != nil {
		return ErrHTTPNotFound
	}
	defer f.Close()

	if _, err = io.Copy(w, f); err != nil {
		return err
	}
	if stat.Mode()&os.ModeNamedPipe == os.ModeNamedPipe {
		os.Remove(file)
	}
	return nil
}

func (s *server) handleClipboardPutRandom(w http.ResponseWriter, r *http.Request) error {
	ctx := context.WithValue(r.Context(), routeCtx{}, []string{randomFileID()})
	return s.handleClipboardPut(w, r.WithContext(ctx))
}

func (s *server) handleClipboardPut(w http.ResponseWriter, r *http.Request) error {
	// Parse request: file ID, stream
	fields := r.Context().Value(routeCtx{}).([]string)
	id := fields[0]
	file, metafile, err := s.getClipboardFile(id)
	if err != nil {
		return ErrHTTPBadRequest
	}

	// Handle empty body
	if r.Body == nil {
		return ErrHTTPBadRequest
	}

	// Check if file exists
	stat, _ := os.Stat(file)
	if stat == nil {
		// File does not exist

		// Check total file count limit
		if err := s.countLimiter.Add(1); err != nil {
			return ErrHTTPTooManyRequests
		}
	} else {
		// File exists

		// Check visitor file count limit
		v := s.getVisitor(r)
		if err := v.countLimiter.Add(1); err != nil {
			return ErrHTTPTooManyRequests
		}

		// File not writable
		m, err := s.readMetaFile(metafile)
		if err != nil {
			return err
		}
		if m.Mode == modeReadOnly {
			return ErrHTTPMethodNotAllowed // TODO test this
		}
	}

	// Always delete file first to avoid awkward FIFO/regular-file behavior
	os.Remove(file)
	os.Remove(metafile)

	mode, err := s.getFileMode(r)
	if err != nil {
		return err
	}

	ttl, err := s.getTTL(r)
	if err != nil {
		return err
	}
	expires := int64(0)
	if ttl > 0 {
		expires = time.Now().Add(ttl).Unix()
	}

	response := &metaFile{
		Mode:    mode,
		Expires: expires,
	}
	mf, err := os.OpenFile(metafile, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer mf.Close()
	if err := json.NewEncoder(mf).Encode(response); err != nil {
		return err
	}
	mf.Close()

	// If this is a stream, make fifo device instead of file if type is set to "fifo".
	// Also, we want to immediately output instructions.
	format := s.outputFormat(r)
	if s.isStream(r) {
		if err := unix.Mkfifo(file, 0600); err != nil {
			s.countLimiter.Sub(1)
			return err
		}
		if err := s.writePutOutput(w, id, expires, ttl, format); err != nil {
			s.countLimiter.Sub(1)
			return err
		}
	}

	// Create new file or truncate existing
	log.Printf("before open %s\n", file)
	f, err := os.OpenFile(file, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	log.Printf("after open %s\n", file)
	if err != nil {
		s.countLimiter.Sub(1)
		return err
	}
	defer f.Close()
	defer s.updateStatsAndExpire()

	// Copy file contents (with file limit & total limit)
	fileSizeLimiter := newLimiter(s.config.FileSizeLimit)
	limitWriter := newLimitWriter(f, fileSizeLimiter, s.sizeLimiter)

	if _, err := io.Copy(limitWriter, r.Body); err != nil {
		os.Remove(file)
		if pe, ok := err.(*fs.PathError); ok {
			err = pe.Err
		}
		if se, ok := err.(*os.SyscallError); ok {
			err = se.Err
		}
		if err == syscall.EPIPE { // "broken pipe", happens when interrupting on receiver-side while streaming
			return ErrHTTPPartialContent
		}
		if err == errLimitReached {
			return ErrHTTPPayloadTooLarge
		}
		return err
	}
	if err := r.Body.Close(); err != nil {
		os.Remove(file)
		return err
	}

	// Output URL, TTL, etc.
	if !s.isStream(r) {
		if err := s.writePutOutput(w, id, expires, ttl, format); err != nil {
			os.Remove(file)
			return err
		}
	}

	return nil
}

func (s *server) writePutOutput(w http.ResponseWriter, id string, expires int64, ttl time.Duration, format string) error {
	println("before put output")
	url, err := s.config.GenerateClipURL(id, ttl) // TODO this is horrible
	if err != nil {
		return err
	}
	// Chrome bug: XMLHttpRequest.onreadystatechange doesn't fire if Content-Type is not set
	// See https://stackoverflow.com/a/4752861/1440785
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set(headerURL, url)
	w.Header().Set(headerExpires, fmt.Sprintf("%d", expires))
	if format == "json" {
		response := &httpResponsePut{
			URL:     url,
			Expires: expires,
		}
		if err := json.NewEncoder(w).Encode(response); err != nil {
			return err
		}
	} else {
		if _, err := w.Write([]byte(url + "\n")); err != nil { // TODO maybe use 'pcopy link' code
			return err
		}
	}
	for i := 0; i < 100; i++ {
		w.Write([]byte("hi there\n"))
	}
	println("before flush")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	println("after flush/put output")
	return nil
}

func (s *server) getFileMode(r *http.Request) (string, error) {
	mode := s.config.FileModesAllowed[0]
	if r.Header.Get(headerFileMode) != "" {
		mode = r.Header.Get(headerFileMode)
	} else if r.URL.Query().Get(queryParamFileMode) != "" {
		mode = r.URL.Query().Get(queryParamFileMode)
	}
	for _, allowed := range s.config.FileModesAllowed {
		if mode == allowed {
			return mode, nil
		}
	}
	return "", ErrHTTPBadRequest
}

func (s *server) getTTL(r *http.Request) (time.Duration, error) {
	var err error
	var ttl time.Duration
	if r.URL.Query().Get(queryParamTTL) != "" {
		ttl, err = parseDuration(r.URL.Query().Get(queryParamTTL))
	} else if r.Header.Get(headerTTL) != "" {
		ttl, err = parseDuration(r.Header.Get(headerTTL))
	} else if s.config.FileExpireAfter > 0 {
		ttl = s.config.FileExpireAfter
	}
	if err != nil {
		return 0, ErrHTTPBadRequest
	}
	if s.config.FileExpireAfter > 0 && ttl > s.config.FileExpireAfter {
		ttl = s.config.FileExpireAfter // TODO test TTL
	}
	return ttl, nil
}

func (s *server) isStream(r *http.Request) bool {
	return r.Header.Get(headerStream) == "yes" || r.URL.Query().Get(queryParamStream) == "1"
}

func (s *server) outputFormat(r *http.Request) string {
	if r.Header.Get(headerFormat) == "json" || r.URL.Query().Get(queryParamFormat) == "json" {
		return "json"
	}
	return "text"
}

func (s *server) getClipboardFile(id string) (string, string, error) {
	for _, reserved := range reservedFiles {
		if id == reserved {
			return "", "", errInvalidFileID
		}
	}
	file := fmt.Sprintf("%s/%s", s.config.ClipboardDir, id)
	meta := fmt.Sprintf("%s/%s:meta", s.config.ClipboardDir, id)
	return file, meta, nil
}

func (s *server) auth(next handlerFnWithErr) handlerFnWithErr {
	return func(w http.ResponseWriter, r *http.Request) error {
		if err := s.authorize(r); err != nil {
			return err
		}
		return next(w, r)
	}
}

func (s *server) authorize(r *http.Request) error {
	if s.config.Key == nil {
		return nil
	}

	auth := r.Header.Get("Authorization")
	if encodedQueryAuth, ok := r.URL.Query()[queryParamAuth]; ok && len(encodedQueryAuth) > 0 {
		queryAuth, err := base64.StdEncoding.DecodeString(encodedQueryAuth[0])
		if err != nil {
			log.Printf("%s - %s %s - cannot decode query auth override", r.RemoteAddr, r.Method, r.RequestURI)
			return ErrHTTPUnauthorized
		}
		auth = string(queryAuth)
	}

	if m := authHmacRegex.FindStringSubmatch(auth); m != nil {
		return s.authorizeHmac(r, m)
	} else if m := authBasicRegex.FindStringSubmatch(auth); m != nil {
		return s.authorizeBasic(r, m)
	} else {
		log.Printf("%s - %s %s - auth header missing", r.RemoteAddr, r.Method, r.RequestURI)
		return ErrHTTPUnauthorized
	}
}

func (s *server) authorizeHmac(r *http.Request, matches []string) error {
	timestamp, err := strconv.Atoi(matches[1])
	if err != nil {
		log.Printf("%s - %s %s - hmac timestamp conversion: %s", r.RemoteAddr, r.Method, r.RequestURI, err.Error())
		return ErrHTTPUnauthorized
	}

	ttlSecs, err := strconv.Atoi(matches[2])
	if err != nil {
		log.Printf("%s - %s %s - hmac ttl conversion: %s", r.RemoteAddr, r.Method, r.RequestURI, err.Error())
		return ErrHTTPUnauthorized
	}

	hash, err := base64.StdEncoding.DecodeString(matches[3])
	if err != nil {
		log.Printf("%s - %s %s - hmac base64 conversion: %s", r.RemoteAddr, r.Method, r.RequestURI, err.Error())
		return ErrHTTPUnauthorized
	}

	// Recalculate HMAC
	data := []byte(fmt.Sprintf("%d:%d:%s:%s", timestamp, ttlSecs, r.Method, r.URL.Path))
	hm := hmac.New(sha256.New, s.config.Key.Bytes)
	if _, err := hm.Write(data); err != nil {
		log.Printf("%s - %s %s - hmac calculation: %s", r.RemoteAddr, r.Method, r.RequestURI, err.Error())
		return ErrHTTPUnauthorized
	}
	rehash := hm.Sum(nil)

	// Compare HMAC in constant time (to prevent timing attacks)
	if subtle.ConstantTimeCompare(hash, rehash) != 1 {
		log.Printf("%s - %s %s - hmac invalid", r.RemoteAddr, r.Method, r.RequestURI)
		return ErrHTTPUnauthorized
	}

	// Compare timestamp (to prevent replay attacks)
	maxAge := defaultMaxAuthAge
	if ttlSecs > 0 {
		maxAge = time.Second * time.Duration(ttlSecs)
	}
	if maxAge > 0 {
		age := time.Since(time.Unix(int64(timestamp), 0))
		if age > maxAge {
			log.Printf("%s - %s %s - hmac request age mismatch", r.RemoteAddr, r.Method, r.RequestURI)
			return ErrHTTPUnauthorized
		}
	}

	return nil
}

func (s *server) authorizeBasic(r *http.Request, matches []string) error {
	userPassBytes, err := base64.StdEncoding.DecodeString(matches[1])
	if err != nil {
		log.Printf("%s - %s %s - basic base64 conversion: %s", r.RemoteAddr, r.Method, r.RequestURI, err.Error())
		return ErrHTTPUnauthorized
	}

	userPassParts := strings.Split(string(userPassBytes), ":")
	if len(userPassParts) != 2 {
		log.Printf("%s - %s %s - basic invalid user/pass format", r.RemoteAddr, r.Method, r.RequestURI)
		return ErrHTTPUnauthorized
	}
	passwordBytes := []byte(userPassParts[1])

	// Compare HMAC in constant time (to prevent timing attacks)
	key := DeriveKey(passwordBytes, s.config.Key.Salt)
	if subtle.ConstantTimeCompare(key.Bytes, s.config.Key.Bytes) != 1 {
		log.Printf("%s - %s %s - basic invalid", r.RemoteAddr, r.Method, r.RequestURI)
		return ErrHTTPUnauthorized
	}

	return nil
}

func (s *server) serverManager() {
	ticker := time.NewTicker(managerTickerInterval)
	for {
		s.updateStatsAndExpire()
		<-ticker.C
	}
}

func (s *server) updateStatsAndExpire() {
	s.Lock()
	defer s.Unlock()

	// Expire visitors from rate visitors map
	for ip, v := range s.visitors {
		if time.Since(v.lastSeen) > visitorExpungeAfter {
			delete(s.visitors, ip)
		}
	}

	// Walk clipboard to update size/count limiters, and expire/delete files
	files, err := ioutil.ReadDir(s.config.ClipboardDir)
	if err != nil {
		log.Printf("error reading clipboard: %s", err.Error())
		return
	}
	numFiles := int64(0)
	totalSize := int64(0)
	for _, f := range files {
		if strings.HasSuffix(f.Name(), ":meta") {
			continue
		}
		if !s.maybeExpire(f) {
			numFiles++
			totalSize += f.Size()
		}
	}
	s.countLimiter.Set(numFiles)
	s.sizeLimiter.Set(totalSize)
	s.printStats()
}

func (s *server) printStats() {
	var countLimit, sizeLimit string
	if s.countLimiter.Limit() == 0 {
		countLimit = "no limit"
	} else {
		countLimit = fmt.Sprintf("max %d", s.countLimiter.Limit())
	}
	if s.sizeLimiter.Limit() == 0 {
		sizeLimit = "no limit"
	} else {
		sizeLimit = fmt.Sprintf("max %s", BytesToHuman(s.sizeLimiter.Limit()))
	}
	log.Printf("files: %d (%s), size: %s (%s), visitors: %d (last 3 minutes)",
		s.countLimiter.Value(), countLimit, BytesToHuman(s.sizeLimiter.Value()), sizeLimit, len(s.visitors))
}

// maybeExpire deletes a file if it has expired and returns true if it did
func (s *server) maybeExpire(info os.FileInfo) bool {
	file := filepath.Join(s.config.ClipboardDir, info.Name())
	metafile := fmt.Sprintf("%s:meta", file)
	if !s.isExpired(info, metafile) {
		return false
	}
	if err := os.Remove(file); err != nil {
		log.Printf("failed to remove clipboard entry after expiry: %s", err.Error())
		return false
	}
	if err := os.Remove(metafile); err != nil {
		log.Printf("failed to remove clipboard meta file after expiry: %s", err.Error())
		return false
	}
	log.Printf("removed expired entry: %s (%s)", info.Name(), BytesToHuman(info.Size()))
	return true
}

// isExpired returns if a file is expired
func (s *server) isExpired(info os.FileInfo, metafile string) bool {
	if mf, err := s.readMetaFile(metafile); err == nil {
		if mf.Expires > 0 {
			return time.Since(time.Unix(mf.Expires, 0)) > 0
		}
		return false
	}
	if s.config.FileExpireAfter == 0 || time.Since(info.ModTime()) <= s.config.FileExpireAfter {
		return false
	}
	return true
}

func (s *server) onlyIfWebUI(next handlerFnWithErr) handlerFnWithErr {
	return func(w http.ResponseWriter, r *http.Request) error {
		if !s.config.WebUI {
			return ErrHTTPBadRequest
		}

		return next(w, r)
	}
}

func (s *server) redirectHTTPS(next handlerFnWithErr) handlerFnWithErr {
	return func(w http.ResponseWriter, r *http.Request) error {
		if r.TLS == nil && s.config.ListenHTTPS != "" {
			newURL := r.URL
			newURL.Host = r.Host
			newURL.Scheme = "https"
			if strings.Contains(newURL.Host, ":") {
				newURL.Host, _, _ = net.SplitHostPort(newURL.Host)
			}
			_, port, _ := net.SplitHostPort(s.config.ListenHTTPS)
			if port != "443" {
				newURL.Host = net.JoinHostPort(newURL.Host, port)
			}
			http.Redirect(w, r, newURL.String(), http.StatusFound)
			return nil
		}
		return next(w, r)
	}
}

// limit wraps all HTTP endpoints and limits API use to a certain number of requests per second.
// This function was taken from https://www.alexedwards.net/blog/how-to-rate-limit-http-requests (MIT).
func (s *server) limit(next handlerFnWithErr) handlerFnWithErr {
	return func(w http.ResponseWriter, r *http.Request) error {
		v := s.getVisitor(r)
		if !v.rateLimiter.Allow() {
			return ErrHTTPTooManyRequests
		}

		return next(w, r)
	}
}

// getVisitor creates or retrieves a rate.Limiter for the given visitor.
// This function was taken from https://www.alexedwards.net/blog/how-to-rate-limit-http-requests (MIT).
func (s *server) getVisitor(r *http.Request) *visitor {
	s.Lock()
	defer s.Unlock()

	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr // This should not happen in real life; only in tests.
	}

	v, exists := s.visitors[ip]
	if !exists {
		v = &visitor{
			newLimiter(visitorCountLimit),
			rate.NewLimiter(visitorRequestsPerSecond, visitorRequestsPerSecondBurst),
			time.Now(),
		}
		s.visitors[ip] = v
		return v
	}

	v.lastSeen = time.Now()
	return v
}

func (s *server) fail(w http.ResponseWriter, r *http.Request, code int, err error) {
	log.Printf("%s - %s %s - %s", r.RemoteAddr, r.Method, r.RequestURI, err.Error())
	w.WriteHeader(code)
	w.Write([]byte(http.StatusText(code)))
}

func (s *server) readMetaFile(metafile string) (*metaFile, error) {
	mf, err := os.Open(metafile)
	if err != nil {
		return nil, err
	}
	defer mf.Close()

	var m metaFile
	if err := json.NewDecoder(mf).Decode(&m); err != nil {
		return nil, err
	}
	return &m, nil
}

type errHTTPNotOK struct {
	code   int
	status string
}

func (e errHTTPNotOK) Error() string {
	return fmt.Sprintf("http: %s", e.status)
}

// ErrHTTPPartialContent is returned when the client interrupts a stream and only partial content was sent
var ErrHTTPPartialContent = &errHTTPNotOK{http.StatusPartialContent, http.StatusText(http.StatusPartialContent)}

// ErrHTTPBadRequest is returned when the request sent by the client was invalid, e.g. invalid file name
var ErrHTTPBadRequest = &errHTTPNotOK{http.StatusBadRequest, http.StatusText(http.StatusBadRequest)}

// ErrHTTPMethodNotAllowed is returned when the file state does not allow the current method, e.g. PUTting a read-only file
var ErrHTTPMethodNotAllowed = &errHTTPNotOK{http.StatusMethodNotAllowed, http.StatusText(http.StatusMethodNotAllowed)}

// ErrHTTPNotFound is returned when a resource is not found on the server
var ErrHTTPNotFound = &errHTTPNotOK{http.StatusNotFound, http.StatusText(http.StatusNotFound)}

// ErrHTTPTooManyRequests is returned when a server-side rate limit has been reached
var ErrHTTPTooManyRequests = &errHTTPNotOK{http.StatusTooManyRequests, http.StatusText(http.StatusTooManyRequests)}

// ErrHTTPPayloadTooLarge is returned when the clipboard/file-size limit has been reached
var ErrHTTPPayloadTooLarge = &errHTTPNotOK{http.StatusRequestEntityTooLarge, http.StatusText(http.StatusRequestEntityTooLarge)}

// ErrHTTPUnauthorized is returned when the client has not sent proper credentials
var ErrHTTPUnauthorized = &errHTTPNotOK{http.StatusUnauthorized, http.StatusText(http.StatusUnauthorized)}

var errListenAddrMissing = errors.New("listen address missing, add 'ListenHTTPS' or 'ListenHTTP' to config or pass --listen-http(s)")
var errKeyFileMissing = errors.New("private key file missing, add 'KeyFile' to config or pass --keyfile")
var errCertFileMissing = errors.New("certificate file missing, add 'CertFile' to config or pass --certfile")
var errClipboardDirNotWritable = errors.New("clipboard dir not writable by user")
var errInvalidFileID = errors.New("invalid file id")
var errNoMatchingRoute = errors.New("no matching route")
