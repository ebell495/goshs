package myhttp

import (
	"archive/zip"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/patrickhener/goshs/internal/myca"
	"github.com/patrickhener/goshs/internal/myclipboard"
	"github.com/patrickhener/goshs/internal/mylog"
	"github.com/patrickhener/goshs/internal/mysock"
	"github.com/patrickhener/goshs/internal/myutils"
	"golang.org/x/net/webdav"
)

const (
	modeWeb = "web"
)

// Static will provide the embedded files as http.FS
//go:embed static
var static embed.FS

type indexTemplate struct {
	Clipboard    *myclipboard.Clipboard
	GoshsVersion string
	Directory    *directory
}

type directory struct {
	RelPath        string
	AbsPath        string
	IsSubdirectory bool
	Back           string
	Content        []item
}

type item struct {
	URI                 string
	Name                string
	IsDir               bool
	IsSymlink           bool
	SymlinkTarget       string
	Ext                 string
	DisplaySize         string
	SortSize            int64
	DisplayLastModified string
	SortLastModified    time.Time
}

// FileServer holds the fileserver information
type FileServer struct {
	IP             string
	Port           int
	WebdavPort     int
	Webroot        string
	SSL            bool
	SelfSigned     bool
	MyKey          string
	MyCert         string
	User           string
	Pass           string
	Version        string
	Fingerprint256 string
	Fingerprint1   string
	UploadOnly     bool
	ReadOnly       bool
	Hub            *mysock.Hub
	Clipboard      *myclipboard.Clipboard
}

type httperror struct {
	ErrorCode    int
	ErrorMessage string
	AbsPath      string
	GoshsVersion string
}

// BasicAuthMiddleware is a middleware to handle the basic auth
func (fs *FileServer) BasicAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="Restricted"`)

		username, password, authOK := r.BasicAuth()
		if !authOK {
			http.Error(w, "Not authorized", http.StatusUnauthorized)
			return
		}

		if username != fs.User || password != fs.Pass {
			http.Error(w, "Not authorized", http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// Start will start the file server
func (fs *FileServer) Start(what string) {
	var addr string
	// Setup routing with gorilla/mux
	mux := mux.NewRouter()

	switch what {
	case modeWeb:
		mux.PathPrefix("/425bda8487e36deccb30dd24be590b8744e3a28a8bb5a57d9b3fcd24ae09ad3c/").HandlerFunc(fs.static)
		// Websocket
		mux.PathPrefix("/14644be038ea0118a1aadfacca2a7d1517d7b209c4b9674ee893b1944d1c2d54/ws").HandlerFunc(fs.socket)
		// Clipboard
		mux.PathPrefix("/14644be038ea0118a1aadfacca2a7d1517d7b209c4b9674ee893b1944d1c2d54/download").HandlerFunc(fs.cbDown)
		mux.PathPrefix("/cf985bddf28fed5d5c53b069d6a6ebe601088ca6e20ec5a5a8438f8e1ffd9390/").HandlerFunc(fs.bulkDownload)
		mux.Methods(http.MethodPost).HandlerFunc(fs.upload)
		mux.PathPrefix("/").HandlerFunc(fs.handler)

		addr = fmt.Sprintf("%+v:%+v", fs.IP, fs.Port)
	case "webdav":
		wdHandler := &webdav.Handler{
			FileSystem: webdav.Dir(fs.Webroot),
			LockSystem: webdav.NewMemLS(),
			Logger: func(r *http.Request, e error) {
				if e != nil && r.Method != "PROPFIND" {
					mylog.Errorf("WEBDAV: %s - - \"%s %s %s\"", r.RemoteAddr, r.Method, r.URL.Path, r.Proto)
					return
				} else if r.Method != "PROPFIND" {
					mylog.Infof("WEBDAV:  %s - - \"%s %s %s\"", r.RemoteAddr, r.Method, r.URL.Path, r.Proto)
				}
			},
		}

		mux.PathPrefix("/").Handler(wdHandler)
		addr = fmt.Sprintf("%+v:%+v", fs.IP, fs.WebdavPort)
	default:
	}

	// construct server
	server := http.Server{
		Addr:    addr,
		Handler: http.AllowQuerySemicolons(mux),
		// Against good practice no timeouts here, otherwise big files would be terminated when downloaded
	}

	// init clipboard
	fs.Clipboard = myclipboard.New()

	// init websocket hub
	fs.Hub = mysock.NewHub(fs.Clipboard)
	go fs.Hub.Run()

	// Check BasicAuth and use middleware
	if fs.User != "" && what == modeWeb {
		if !fs.SSL {
			mylog.Warnf("You are using basic auth without SSL. Your credentials will be transferred in cleartext. Consider using -s, too.")
		}
		mylog.Infof("Using basic auth with user '%s' and password '%s'", fs.User, fs.Pass)
		// Use middleware
		mux.Use(fs.BasicAuthMiddleware)
	}

	// Check if ssl
	if fs.SSL {
		// Check if selfsigned
		if fs.SelfSigned {
			serverTLSConf, fingerprint256, fingerprint1, err := myca.Setup()
			if err != nil {
				mylog.Fatalf("Unable to start SSL enabled server: %+v\n", err)
			}
			server.TLSConfig = serverTLSConf
			fs.Fingerprint256 = fingerprint256
			fs.Fingerprint1 = fingerprint1
			fs.logStart(what)

			mylog.Panic(server.ListenAndServeTLS("", ""))
		} else {
			if fs.MyCert == "" || fs.MyKey == "" {
				mylog.Fatal("You need to provide server.key and server.crt if -s and not -ss")
			}

			fingerprint256, fingerprint1, err := myca.ParseAndSum(fs.MyCert)
			if err != nil {
				mylog.Fatalf("Unable to start SSL enabled server: %+v\n", err)
			}
			fs.Fingerprint256 = fingerprint256
			fs.Fingerprint1 = fingerprint1
			fs.logStart(what)

			mylog.Panic(server.ListenAndServeTLS(fs.MyCert, fs.MyKey))
		}
	} else {
		fs.logStart(what)
		mylog.Panic(server.ListenAndServe())
	}
}

// socket will handle the socket connection
func (fs *FileServer) socket(w http.ResponseWriter, req *http.Request) {
	mysock.ServeWS(fs.Hub, w, req)
}

// clipboardAdd will handle the add request for adding text to the clipboard
func (fs *FileServer) cbDown(w http.ResponseWriter, req *http.Request) {
	filename := fmt.Sprintf("%+v-clipboard.json", int32(time.Now().Unix()))
	contentDisposition := fmt.Sprintf("attachment; filename=\"%s\"", filename)
	// Handle as download
	w.Header().Add("Content-Type", "application/octet-stream")
	w.Header().Add("Content-Disposition", contentDisposition)
	content, err := fs.Clipboard.Download()
	if err != nil {
		fs.handleError(w, req, err, 500)
	}

	if _, err := w.Write(content); err != nil {
		mylog.Errorf("Error writing response to browser: %+v", err)
	}
}

// static will give static content for style and function
func (fs *FileServer) static(w http.ResponseWriter, req *http.Request) {
	// Check which file to serve
	upath := req.URL.Path
	staticPath := strings.SplitAfterN(upath, "/", 3)[2]
	path := "static/" + staticPath
	// Load file with parcello
	staticFile, err := static.ReadFile(path)
	if err != nil {
		mylog.Errorf("static file: %+v cannot be loaded: %+v", path, err)
	}

	// Get mimetype from extension
	contentType := myutils.MimeByExtension(staticPath)

	// Set mimetype and deliver to browser
	w.Header().Add("Content-Type", contentType)
	if _, err := w.Write(staticFile); err != nil {
		mylog.Errorf("Error writing response to browser: %+v", err)
	}
}

// handler is the function which actually handles dir or file retrieval
func (fs *FileServer) handler(w http.ResponseWriter, req *http.Request) {
	// Get url so you can extract Headline and title
	upath := req.URL.Path

	// Ignore default browser call to /favicon.ico
	if upath == "/favicon.ico" {
		return
	}
	upath = path.Clean(upath)
	upath = filepath.Clean(upath)

	mylog.Debugf("Cleaned upath is: %+v", upath)

	// Define absolute path
	open := fs.Webroot + upath

	// Check if you are in a dir
	// disable G304 (CWE-22): Potential file inclusion via variable
	// as we want a file inclusion here
	// #nosec G304
	file, err := os.Open(open)
	if os.IsNotExist(err) {
		fs.handleError(w, req, err, http.StatusNotFound)
		return
	}
	if os.IsPermission(err) {
		fs.handleError(w, req, err, http.StatusInternalServerError)
		return
	}
	if err != nil {
		// Handle general error
		mylog.Info(err)
		return
	}
	// disable G307 (CWE-703): Deferring unsafe method "Close" on type "*os.File"
	// #nosec G307
	defer file.Close()

	// Log request
	mylog.LogRequest(req, http.StatusOK)

	// Switch and check if dir
	stat, _ := file.Stat()
	if stat.IsDir() {
		fs.processDir(w, req, file, upath)
	} else {
		fs.sendFile(w, req, file)
	}
}

// upload handles the POST request to upload files
func (fs *FileServer) upload(w http.ResponseWriter, req *http.Request) {
	if fs.ReadOnly {
		fs.handleError(w, req, fmt.Errorf("%s", "Upload not allowed due to 'read only' option"), http.StatusForbidden)
		return
	}
	// Get url so you can extract Headline and title
	upath := req.URL.Path

	// construct target path
	targetpath := strings.Split(upath, "/")
	targetpath = targetpath[:len(targetpath)-1]
	target := strings.Join(targetpath, "/")

	// Parse request
	if err := req.ParseMultipartForm(10 << 20); err != nil {
		mylog.Errorf("parsing multipart request: %+v", err)
		return
	}

	// Get ref to the parsed multipart form
	m := req.MultipartForm

	for _, f := range m.File {
		file, err := f[0].Open()
		if err != nil {
			mylog.Errorf("retrieving the file: %+v\n", err)
		}
		defer file.Close()

		filename := f[0].Filename

		// Sanitize filename (No path traversal)
		filenameSlice := strings.Split(filename, "/")
		filenameClean := filenameSlice[len(filenameSlice)-1]

		// Construct absolute savepath
		savepath := fmt.Sprintf("%s%s/%s", fs.Webroot, target, filenameClean)

		// Create file to write to
		// disable G304 (CWE-22): Potential file inclusion via variable
		// as we want a file inclusion here
		// #nosec G304
		if _, err := os.Create(savepath); err != nil {
			mylog.Errorf("Not able to create file on disk")
			fs.handleError(w, req, err, http.StatusInternalServerError)
		}

		// Read file from post body
		fileBytes, err := ioutil.ReadAll(file)
		if err != nil {
			mylog.Errorf("Not able to read file from request")
			fs.handleError(w, req, err, http.StatusInternalServerError)
		}

		// Write file to disk
		if err := ioutil.WriteFile(savepath, fileBytes, os.ModePerm); err != nil {
			mylog.Errorf("Not able to write file to disk")
			fs.handleError(w, req, err, http.StatusInternalServerError)
		}
	}

	// Log request
	mylog.LogRequest(req, http.StatusOK)

	// Redirect back from where we came from
	http.Redirect(w, req, target, http.StatusSeeOther)
}

// bulkDownload will provide zip archived download bundle of multiple selected files
func (fs *FileServer) bulkDownload(w http.ResponseWriter, req *http.Request) {
	if fs.UploadOnly {
		fs.handleError(w, req, fmt.Errorf("%s", "Bulk download not allowed due to 'upload only' option"), http.StatusForbidden)
		return
	}
	// make slice and query files from request
	var filesCleaned []string
	files := req.URL.Query()["file"]

	// Handle if no files are selected
	if len(files) == 0 {
		fs.handleError(w, req, errors.New("you need to select a file before you can download a zip archive"), 404)
	}

	// Clean file paths and fill slice
	// Also sanitize path (No path traversal)
	// If .. in single string just skip file
	for _, file := range files {
		fileCleaned, _ := url.QueryUnescape(file)
		if strings.Contains(fileCleaned, "..") {
			// Just skip this file
			continue
		}
		filesCleaned = append(filesCleaned, fileCleaned)
	}

	// Construct filename to download
	filename := fmt.Sprintf("%+v_goshs_download.zip", int32(time.Now().Unix()))

	// Set header and serve file
	contentDispo := fmt.Sprintf("attachment; filename=\"%s\"", filename)
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", contentDispo)
	w.Header().Set("Content-Transfer-Encoding", "binary")
	w.Header().Set("Expires", "0")

	// Define Zip writer
	resultZip := zip.NewWriter(w)
	defer resultZip.Close()

	// Path walker for recursion
	walker := func(filepath string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}

		// disable G304 (CWE-22): Potential file inclusion via variable
		// as we want a file inclusion here
		// #nosec G304
		file, err := os.Open(filepath)
		if err != nil {
			return err
		}
		// disable G307 (CWE-703): Deferring unsafe method "Close" on type "*os.File"
		// #nosec G307
		defer file.Close()

		// filepath is fs.Webroot + file relative path
		// this would result in a lot of nested folders
		// so we are stripping fs.Webroot again from the structure of the zip file
		// Leaving us with the relative path of the file
		zippath := strings.ReplaceAll(filepath, fs.Webroot, "")
		f, err := resultZip.Create(zippath[1:])
		if err != nil {
			return err
		}

		_, err = io.Copy(f, file)
		if err != nil {
			return err
		}

		return nil
	}

	// Loop over files and add to zip
	for _, file := range filesCleaned {
		err := filepath.Walk(path.Join(fs.Webroot, file), walker)
		if err != nil {
			mylog.Errorf("creating zip file: %+v", err)
		}
	}

	// Close Zip Writer and Flush to http.ResponseWriter
	if err := resultZip.Close(); err != nil {
		mylog.Error(err)
	}
}

func (fs *FileServer) processDir(w http.ResponseWriter, req *http.Request, file *os.File, relpath string) {
	// Read directory FileInfo
	fis, err := file.Readdir(-1)
	if err != nil {
		fs.handleError(w, req, err, http.StatusNotFound)
		return
	}

	// Create empty slice
	items := make([]item, 0, len(fis))
	// Iterate over FileInfo of dir
	for _, fi := range fis {
		item := item{}
		// Need to set this up here for directories to work
		item.Name = fi.Name()
		item.Ext = strings.ToLower(myutils.ReturnExt(fi.Name()))
		// Add / to name if dir
		if fi.IsDir() {
			// Check if special path exists as dir on disk and do not add
			if myutils.CheckSpecialPath(fi.Name()) {
				continue
			}
			item.Name += "/"
			item.IsDir = true
			item.Ext = ""
		}
		// Set item fields
		item.URI = url.PathEscape(path.Join(relpath, fi.Name()))
		item.DisplaySize = myutils.ByteCountDecimal(fi.Size())
		item.SortSize = fi.Size()
		item.DisplayLastModified = fi.ModTime().Format("Mon Jan _2 15:04:05 2006")
		item.SortLastModified = fi.ModTime()
		// Check and resolve symlink
		if fi.Mode()&os.ModeSymlink != 0 {
			item.IsSymlink = true
			item.SymlinkTarget, err = os.Readlink(path.Join(fs.Webroot, relpath, fi.Name()))
			if err != nil {
				mylog.Errorf("resolving symlink: %+v", err)
			}
		}
		// Add to items slice
		items = append(items, item)
	}

	// Sort slice all lowercase
	sort.Slice(items, func(i, j int) bool {
		return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name)
	})

	// Template parsing and writing to browser
	indexFile, err := static.ReadFile("static/templates/index.html")
	if err != nil {
		mylog.Errorf("opening embedded file: %+v", err)
	}

	// Windows upload compatibility
	if relpath == "\\" {
		relpath = "/"
	}

	// Construct directory for template
	d := &directory{
		RelPath: relpath,
		AbsPath: filepath.Join(fs.Webroot, relpath),
		Content: items,
	}
	if relpath != "/" {
		d.IsSubdirectory = true
		pathSlice := strings.Split(relpath, "/")
		if len(pathSlice) > 2 {
			pathSlice = pathSlice[1 : len(pathSlice)-1]

			var backString string
			for _, part := range pathSlice {
				backString += "/" + part
			}
			d.Back = backString
		} else {
			d.Back = "/"
		}
	} else {
		d.IsSubdirectory = false
	}

	// upload only mode empty directory
	if fs.UploadOnly {
		d = &directory{}
	}

	// Construct template
	tem := &indexTemplate{
		Directory:    d,
		GoshsVersion: fs.Version,
		Clipboard:    fs.Clipboard,
	}

	t := template.New("index")
	if _, err := t.Parse(string(indexFile)); err != nil {
		mylog.Errorf("Error parsing template: %+v", err)
	}
	if err := t.Execute(w, tem); err != nil {
		mylog.Errorf("Error executing template: %+v", err)
	}
}

func (fs *FileServer) sendFile(w http.ResponseWriter, req *http.Request, file *os.File) {
	if fs.UploadOnly {
		fs.handleError(w, req, fmt.Errorf("%s", "Download not allowed due to 'upload only' option"), http.StatusForbidden)
		return
	}
	// Extract download parameter
	download := req.URL.Query()
	if _, ok := download["download"]; ok {
		stat, err := file.Stat()
		if err != nil {
			mylog.Errorf("reading file stats for download: %+v", err)
		}
		contentDisposition := fmt.Sprintf("attachment; filename=\"%s\"", stat.Name())
		// Handle as download
		w.Header().Add("Content-Type", "application/octet-stream")
		w.Header().Add("Content-Disposition", contentDisposition)
		w.Header().Add("Content-Length", fmt.Sprintf("%d", stat.Size()))
		if _, err := io.Copy(w, file); err != nil {
			mylog.Errorf("Error writing response to browser: %+v", err)
		}
	} else {
		// Write to browser
		if _, err := io.Copy(w, file); err != nil {
			mylog.Errorf("Error writing response to browser: %+v", err)
		}
	}
}

func (fs *FileServer) handleError(w http.ResponseWriter, req *http.Request, err error, status int) {
	// Set header to status
	w.WriteHeader(status)

	// Define empty error
	var e httperror

	// Log to console
	mylog.LogRequest(req, status)

	// Construct error for template filling
	e.ErrorCode = status
	e.ErrorMessage = err.Error()
	e.AbsPath = path.Join(fs.Webroot, req.URL.Path)
	e.GoshsVersion = fs.Version

	// Template handling
	file, err := static.ReadFile("static/templates/error.html")
	if err != nil {
		mylog.Errorf("opening embedded file: %+v", err)
	}
	t := template.New("error")
	if _, err := t.Parse(string(file)); err != nil {
		mylog.Errorf("parsing the template: %+v", err)
	}
	if err := t.Execute(w, e); err != nil {
		mylog.Errorf("executing the template: %+v", err)
	}
}

func (fs *FileServer) logStart(what string) {
	var interfaceAdresses map[string]string
	var err error
	if what == modeWeb {
		if fs.IP == "0.0.0.0" {
			interfaceAdresses, err = myutils.GetAllIPAdresses()
			if err != nil {
				mylog.Errorf("There has been an error fetching the interface addresses: %+v\n", err)
			}
			for k, v := range interfaceAdresses {
				mylog.Infof("Serving on interface %s bound to %s:%+v\n", k, v, fs.Port)
			}
		} else {
			mylog.Infof("Serving on %s:%+v\n", fs.IP, fs.Port)
		}
	}

	protocol := "HTTP"
	if fs.SSL {
		protocol = "HTTPS"
	}

	switch what {
	case modeWeb:
		if fs.SSL {
			// Check if selfsigned
			if fs.SelfSigned {
				mylog.Infof("Serving %s from %+v with ssl enabled and self-signed certificate\n", protocol, fs.Webroot)
				mylog.Warn("Be sure to check the fingerprint of certificate")
				mylog.Infof("SHA-256 Fingerprint: %+v\n", fs.Fingerprint256)
				mylog.Infof("SHA-1   Fingerprint: %+v\n", fs.Fingerprint1)
			} else {
				mylog.Infof("Serving %s from %+v with ssl enabled server key: %+v, server cert: %+v\n", protocol, fs.Webroot, fs.MyKey, fs.MyCert)
				mylog.Info("You provided a certificate and might want to check the fingerprint nonetheless")
				mylog.Infof("SHA-256 Fingerprint: %+v\n", fs.Fingerprint256)
				mylog.Infof("SHA-1   Fingerprint: %+v\n", fs.Fingerprint1)
			}
		} else {
			mylog.Infof("Serving %s from %+v\n", protocol, fs.Webroot)
		}
	case "webdav":
		if fs.SSL {
			// Check if selfsigned
			if fs.SelfSigned {
				mylog.Infof("Serving WEBDAV on %+v:%+v from %+v with ssl enabled and self-signed certificate\n", fs.IP, fs.WebdavPort, fs.Webroot)
				mylog.Warn("WARNING! Be sure to check the fingerprint of certificate")
				mylog.Infof("SHA-256 Fingerprint: %+v\n", fs.Fingerprint256)
				mylog.Infof("SHA-1   Fingerprint: %+v\n", fs.Fingerprint1)
			} else {
				mylog.Infof("Serving WEBDAV on %+v:%+v from %+v with ssl enabled server key: %+v, server cert: %+v\n", fs.IP, fs.WebdavPort, fs.Webroot, fs.MyKey, fs.MyCert)
				mylog.Info("INFO! You provided a certificate and might want to check the fingerprint nonetheless")
				mylog.Infof("SHA-256 Fingerprint: %+v\n", fs.Fingerprint256)
				mylog.Infof("SHA-1   Fingerprint: %+v\n", fs.Fingerprint1)
			}
		} else {
			mylog.Infof("Serving WEBDAV on %+v:%+v from %+v\n", fs.IP, fs.WebdavPort, fs.Webroot)
		}
	default:
	}
}
