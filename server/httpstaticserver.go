package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"regexp"

	"github.com/codeskyblue/gohttpserver/auth"
	"github.com/codeskyblue/gohttpserver/common"
	"github.com/codeskyblue/gohttpserver/secret"
	"github.com/dgrijalva/jwt-go"
	"github.com/go-yaml/yaml"
	"github.com/gorilla/mux"
	"github.com/shogo82148/androidbinary/apk"
)

type ApkInfo struct {
	PackageName  string `json:"packageName"`
	MainActivity string `json:"mainActivity"`
	Version      struct {
		Code int    `json:"code"`
		Name string `json:"name"`
	} `json:"version"`
}

type IndexFileItem struct {
	Path string
	// Info os.FileInfo
	Info fs.FileInfo
}

type Directory struct {
	size  map[string]int64
	mutex *sync.RWMutex
}

type HTTPStaticServer struct {
	Root            string
	Prefix          string
	PrefixReflect   []*regexp.Regexp
	PinRoot         bool
	Token           string
	Upload          bool
	Delete          bool
	Folder          bool
	Download        bool
	Title           string
	Theme           string
	PlistProxy      string
	GoogleTrackerID string
	AuthType        string

	indexes []IndexFileItem
	m       *mux.Router
	bufPool sync.Pool // use sync.Pool caching buf to reduce gc ratio
}

const AccessUpload = 0b10000000
const AccessDelete = 0b01000000
const AccessFolder = 0b00100000
const AccessDownload = 0b00010000

func NewHTTPStaticServer(root string) *HTTPStaticServer {
	root = filepath.ToSlash(filepath.Clean(root))
	if !strings.HasSuffix(root, "/") {
		root = root + "/"
	}
	log.Printf("root path: %s\n", root)
	m := mux.NewRouter()
	s := &HTTPStaticServer{
		Root:  root,
		Theme: "black",
		m:     m,
		bufPool: sync.Pool{
			New: func() interface{} { return make([]byte, 32*1024) },
		},
	}

	go func() {
		time.Sleep(1 * time.Second)
		for {
			startTime := time.Now()
			log.Println("Started making search index")
			s.makeIndex()
			log.Printf("Completed search index in %v", time.Since(startTime))
			//time.Sleep(time.Second * 1)
			time.Sleep(time.Minute * 10)
		}
	}()

	m.HandleFunc("/{path:.*}", s.hIndex).Methods("GET", "HEAD")
	m.HandleFunc("/{path:.*}", s.hUploadOrMkdir).Methods("POST")
	m.HandleFunc("/{path:.*}", s.hDelete).Methods("DELETE")
	return s
}

func (s *HTTPStaticServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.m.ServeHTTP(w, r)
}

func startsWith(s, prefix string) bool {
	if len(s) < len(prefix) {
		return false
	}
	return s[:len(prefix)] == prefix
}

func endsWith(s, suffix string) bool {
	if len(s) < len(suffix) {
		return false
	}
	return s[len(s)-len(suffix):] == suffix
}

// Return real path with Seperator(/)
func (s *HTTPStaticServer) getRealPath(r *http.Request) string {
	path := mux.Vars(r)["path"]
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	path = filepath.Clean(path) // prevent .. for safe issues
	relativePath, err := filepath.Rel(s.Prefix, path)
	if err != nil {
		relativePath = path
	}
	realPath := filepath.Join(s.Root, relativePath)
	return filepath.ToSlash(realPath)
}

func (s *HTTPStaticServer) getTokenClaims(r *http.Request) (claims jwt.MapClaims, err error) {
	token := r.Header.Get("X-Requested-File-Server-Token")
	if token == "" {
		return nil, errors.New("token is empty")
	}

	claims, err = secret.ParseJWT(common.PublicKeyPath, token)
	if err != nil {
		fmt.Println("err: ", err)
		return nil, err
	}

	return claims, nil
}

func (s *HTTPStaticServer) getAccessFromToken(r *http.Request) (*UserControl, error) {
	claims, err := s.getTokenClaims(r)

	if err != nil {
		return nil, err
	}

	access := &UserControl{}
	if val, ok := claims["upload"]; ok {
		access.Upload = val.(bool)
	}

	if val, ok := claims["delete"]; ok {
		access.Delete = val.(bool)
	}

	if val, ok := claims["folder"]; ok {
		access.Folder = val.(bool)
	}

	if val, ok := claims["download"]; ok {
		access.Download = val.(bool)
	}

	return access, nil
}

func (s *HTTPStaticServer) checkToken(w http.ResponseWriter, r *http.Request, path string) (isok bool, root string) {
	claims, err := s.getTokenClaims(r)

	if s.PinRoot {
		if err != nil {
			return false, ""
		}

		root = claims["root"].(string)

		if !startsWith(path, root) {
			queryParams := r.URL.Query()
			http.Redirect(w, r, "/"+root+"?"+queryParams.Encode(), http.StatusFound)
			return false, ""
		}
	}
	return true, root
}

func (s *HTTPStaticServer) hIndex(w http.ResponseWriter, r *http.Request) {
	path := mux.Vars(r)["path"]

	realPath := s.getRealPath(r)
	if r.FormValue("json") == "true" {
		ok, root := s.checkToken(w, r, path)
		if !ok {
			return
		}
		if !startsWith(root, "/") {
			root = "/" + root
		}

		if endsWith(root, "/") {
			root = root[:len(root)-1]
		}

		if s.PinRoot {
			re, _ := regexp.Compile(root)
			s.PrefixReflect = []*regexp.Regexp{re}
			access, err := s.getAccessFromToken(r)
			if err != nil {
				log.Println("err: ", err)
				return
			}
			s.Upload = access.Upload
			s.Delete = access.Delete
			s.Folder = access.Folder
			s.Download = access.Download
		}
		s.hJSONList(w, r)
		return
	}

	if r.FormValue("op") == "info" {
		s.hInfo(w, r)
		return
	}

	if r.FormValue("op") == "archive" {
		s.hZip(w, r)
		return
	}

	log.Println("GET", path, realPath)

	if r.FormValue("raw") == "false" || common.IsDir(realPath) {
		if r.Method == "HEAD" {
			return
		}

		if s.PinRoot {
			_, err := os.Lstat(common.PrivateKeyPath)

			if err != nil {
				_, _, err = secret.CreatePEM(common.SecretPath)
				if err != nil {
					log.Println("create pem error: ", err)
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
			}

			claims := jwt.MapClaims{
				"root":     path,
				"upload":   false,
				"download": false,
				"delete":   false,
				"folder":   false,
			}

			accessStr := r.FormValue("access")
			if accessStr != "" {
				access, err := strconv.Atoi(accessStr)
				if err == nil {
					claims["upload"] = (access & AccessUpload) == AccessUpload
					claims["download"] = (access & AccessDownload) == AccessDownload
					claims["delete"] = (access & AccessDelete) == AccessDelete
					claims["folder"] = (access & AccessFolder) == AccessFolder
				}
			}

			token, err := secret.CreateJWT(common.PrivateKeyPath, claims)
			if err != nil {
				log.Println("create jwt error: ", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			s.Token = token
			renderHTML(w, "assets/index.html", s)
			return
		}

		renderHTML(w, "assets/index.html", s)
	} else {
		if filepath.Base(path) == common.YAMLCONF {
			auth := s.readAccessConf(realPath)
			if !auth.Delete {
				http.Error(w, "Security warning, not allowed to read", http.StatusForbidden)
				return
			}
		}
		if r.FormValue("download") == "true" {
			w.Header().Set("Content-Disposition", "attachment; filename="+strconv.Quote(filepath.Base(path)))
		}
		http.ServeFile(w, r, realPath)
	}
}

func (s *HTTPStaticServer) hDelete(w http.ResponseWriter, req *http.Request) {
	path := mux.Vars(req)["path"]
	realPath := s.getRealPath(req)
	// path = filepath.Clean(path) // for safe reason, prevent path contain ..
	auth := s.readAccessConf(realPath)
	if !auth.canDelete(req) {
		http.Error(w, "Delete forbidden", http.StatusForbidden)
		return
	}

	// TODO: path safe check
	err := os.RemoveAll(realPath)
	if err != nil {
		pathErr, ok := err.(*os.PathError)
		if ok {
			http.Error(w, pathErr.Op+" "+path+": "+pathErr.Err.Error(), 500)
		} else {
			http.Error(w, err.Error(), 500)
		}
		return
	}
	w.Write([]byte("Success"))
}

func (s *HTTPStaticServer) hUploadOrMkdir(w http.ResponseWriter, req *http.Request) {
	dirpath := s.getRealPath(req)

	// check auth
	auth := s.readAccessConf(dirpath)
	if !auth.canUpload(req) {
		http.Error(w, "Upload forbidden", http.StatusForbidden)
		return
	}

	file, header, err := req.FormFile("file")

	if _, err := os.Stat(dirpath); os.IsNotExist(err) {
		if err := os.MkdirAll(dirpath, os.ModePerm); err != nil {
			log.Println("Create directory:", err)
			http.Error(w, "Directory create "+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	if file == nil { // only mkdir
		w.Header().Set("Content-Type", "application/json;charset=utf-8")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":     true,
			"destination": dirpath,
		})
		return
	}

	if err != nil {
		log.Println("Parse form file:", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer func() {
		file.Close()
		req.MultipartForm.RemoveAll() // Seen from go source code, req.MultipartForm not nil after call FormFile(..)
	}()

	filename := req.FormValue("filename")
	if filename == "" {
		filename = header.Filename
	}
	if err := checkFilename(filename); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}

	dstPath := filepath.Join(dirpath, filename)

	// Large file (>32MB) will store in tmp directory
	// The quickest operation is call os.Move instead of os.Copy
	// Note: it seems not working well, os.Rename might be failed

	var copyErr error
	// if osFile, ok := file.(*os.File); ok && fileExists(osFile.Name()) {
	// 	tmpUploadPath := osFile.Name()
	// 	osFile.Close() // Windows can not rename opened file
	// 	log.Printf("Move %s -> %s", tmpUploadPath, dstPath)
	// 	copyErr = os.Rename(tmpUploadPath, dstPath)
	// } else {
	dst, err := os.Create(dstPath)
	if err != nil {
		log.Println("Create file:", err)
		http.Error(w, "File create "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Note: very large size file might cause poor performance
	// _, copyErr = io.Copy(dst, file)
	buf := s.bufPool.Get().([]byte)
	defer s.bufPool.Put(&buf)
	_, copyErr = io.CopyBuffer(dst, file, buf)
	dst.Close()
	// }
	if copyErr != nil {
		log.Println("Handle upload file:", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json;charset=utf-8")

	if req.FormValue("unzip") == "true" {
		err = common.UnzipFile(dstPath, dirpath)
		os.Remove(dstPath)
		message := "success"
		if err != nil {
			message = err.Error()
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":     err == nil,
			"description": message,
		})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":     true,
		"destination": dstPath,
	})
}

type FileJSONInfo struct {
	Name    string      `json:"name"`
	Type    string      `json:"type"`
	Size    int64       `json:"size"`
	Path    string      `json:"path"`
	ModTime int64       `json:"mtime"`
	Extra   interface{} `json:"extra,omitempty"`
}

// path should be absolute
func parseApkInfo(path string) (ai *ApkInfo) {
	defer func() {
		if err := recover(); err != nil {
			log.Println("parse-apk-info panic:", err)
		}
	}()
	apkf, err := apk.OpenFile(path)
	if err != nil {
		return
	}
	ai = &ApkInfo{}
	ai.MainActivity, _ = apkf.MainActivity()
	ai.PackageName = apkf.PackageName()
	ai.Version.Code = apkf.Manifest().VersionCode
	ai.Version.Name = apkf.Manifest().VersionName
	return
}

func (s *HTTPStaticServer) hInfo(w http.ResponseWriter, r *http.Request) {
	path := mux.Vars(r)["path"]
	relPath := s.getRealPath(r)

	fi, err := os.Stat(relPath)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	fji := &FileJSONInfo{
		Name:    fi.Name(),
		Size:    fi.Size(),
		Path:    path,
		ModTime: fi.ModTime().UnixNano() / 1e6,
	}
	ext := filepath.Ext(path)
	switch ext {
	case ".md":
		fji.Type = "markdown"
	case ".apk":
		fji.Type = "apk"
		fji.Extra = parseApkInfo(relPath)
	case "":
		fji.Type = "dir"
	default:
		fji.Type = "text"
	}
	data, _ := json.Marshal(fji)
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

func (s *HTTPStaticServer) hZip(w http.ResponseWriter, r *http.Request) {
	common.CompressToZip(w, s.getRealPath(r))
}

// func (s *HTTPStaticServer) hUnzip(w http.ResponseWriter, r *http.Request) {
// 	vars := mux.Vars(r)
// 	zipPath, path := vars["zip_path"], vars["path"]
// 	ctype := mime.TypeByExtension(filepath.Ext(path))
// 	if ctype != "" {
// 		w.Header().Set("Content-Type", ctype)
// 	}
// 	err := ExtractFromZip(filepath.Join(s.Root, zipPath), path, w)
// 	if err != nil {
// 		http.Error(w, err.Error(), 500)
// 		return
// 	}
// }

// func combineURL(r *http.Request, path string) *url.URL {
// 	return &url.URL{
// 		Scheme: r.URL.Scheme,
// 		Host:   r.Host,
// 		Path:   path,
// 	}
// }

// func (s *HTTPStaticServer) hFileOrDirectory(w http.ResponseWriter, r *http.Request) {
// 	http.ServeFile(w, r, s.getRealPath(r))
// }

type HTTPFileInfo struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Type    string `json:"type"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"mtime"`
}

type AccessTable struct {
	Regex string `yaml:"regex"`
	Allow bool   `yaml:"allow"`
}

type UserControl struct {
	Email string
	// Access bool
	Upload   bool
	Delete   bool
	Folder   bool
	Download bool
	Token    string
}

type AccessConf struct {
	Upload       bool          `yaml:"upload" json:"upload"`
	Delete       bool          `yaml:"delete" json:"delete"`
	Folder       bool          `yaml:"folder" json:"folder"`
	Download     bool          `yaml:"download" json:"download"`
	Users        []UserControl `yaml:"users" json:"users"`
	AccessTables []AccessTable `yaml:"accessTables"`
}

var reCache = make(map[string]*regexp.Regexp)

func (c *AccessConf) canAccess(fileName string) bool {
	for _, table := range c.AccessTables {
		pattern, ok := reCache[table.Regex]
		if !ok {
			pattern, _ = regexp.Compile(table.Regex)
			reCache[table.Regex] = pattern
		}
		// skip wrong format regex
		if pattern == nil {
			continue
		}
		if pattern.MatchString(fileName) {
			return table.Allow
		}
	}
	return true
}

func (c *AccessConf) canDelete(r *http.Request) bool {
	session, err := auth.Store.Get(r, auth.DefaultSessionName)
	if err != nil {
		return c.Delete
	}
	val := session.Values["user"]
	if val == nil {
		return c.Delete
	}
	userInfo := val.(*auth.UserInfo)
	for _, rule := range c.Users {
		if rule.Email == userInfo.Email {
			return rule.Delete
		}
	}
	return c.Delete
}

func (c *AccessConf) canNewFolder(r *http.Request) bool {
	session, err := auth.Store.Get(r, auth.DefaultSessionName)
	if err != nil {
		return c.Folder
	}
	val := session.Values["user"]
	if val == nil {
		return c.Folder
	}
	userInfo := val.(*auth.UserInfo)
	for _, rule := range c.Users {
		if rule.Email == userInfo.Email {
			return rule.Folder
		}
	}
	return c.Folder
}

func (c *AccessConf) canDownload(r *http.Request) bool {
	session, err := auth.Store.Get(r, auth.DefaultSessionName)
	if err != nil {
		return c.Download
	}
	val := session.Values["user"]
	if val == nil {
		return c.Download
	}
	userInfo := val.(*auth.UserInfo)
	for _, rule := range c.Users {
		if rule.Email == userInfo.Email {
			return rule.Download
		}
	}
	return c.Download
}

func (c *AccessConf) canUploadByToken(token string) bool {
	for _, rule := range c.Users {
		if rule.Token == token {
			return rule.Upload
		}
	}
	return c.Upload
}

func (c *AccessConf) canUpload(r *http.Request) bool {
	token := r.FormValue("token")
	if token != "" {
		return c.canUploadByToken(token)
	}
	session, err := auth.Store.Get(r, auth.DefaultSessionName)
	if err != nil {
		return c.Upload
	}
	val := session.Values["user"]
	if val == nil {
		return c.Upload
	}
	userInfo := val.(*auth.UserInfo)

	for _, rule := range c.Users {
		if rule.Email == userInfo.Email {
			return rule.Upload
		}
	}
	return c.Upload
}

type ResponseConfigs struct {
	PrefixReflect []string `json:"prefixReflect"`
}

func (s *HTTPStaticServer) hJSONList(w http.ResponseWriter, r *http.Request) {
	requestPath := mux.Vars(r)["path"]
	realPath := s.getRealPath(r)
	search := r.FormValue("search")
	auth := s.readAccessConf(realPath)
	auth.Upload = auth.canUpload(r)
	auth.Delete = auth.canDelete(r)
	auth.Folder = auth.canNewFolder(r)
	auth.Download = auth.canDownload(r)

	// path string -> info os.FileInfo
	fileInfoMap := make(map[string]os.FileInfo, 0)
	// fileInfoMap := make(map[string]fs.FileInfo, 0)
	dirInfoMap := make(map[string]os.DirEntry, 0)
	if search != "" {
		results := s.findIndex(search)
		if len(results) > 50 { // max 50
			results = results[:50]
		}
		for _, item := range results {
			if strings.HasPrefix(item.Path, requestPath) {
				fileInfoMap[item.Path] = item.Info
			}
		}
	} else {
		infos, err := os.ReadDir(realPath)
		// infos, err := ioutil.ReadDir(realPath)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		for _, info := range infos {
			// fileInfoMap[filepath.Join(requestPath, info.Name())] = info
			dirInfoMap[filepath.Join(requestPath, info.Name())] = info
		}
	}

	// turn file list -> json
	lrs := make([]HTTPFileInfo, 0)
	for path, info := range fileInfoMap {
		if !auth.canAccess(info.Name()) {
			continue
		}
		lr := HTTPFileInfo{
			Name:    info.Name(),
			Path:    path,
			ModTime: info.ModTime().UnixNano() / 1e6,
		}
		if search != "" {
			name, err := filepath.Rel(requestPath, path)
			if err != nil {
				log.Println(requestPath, path, err)
			}
			lr.Name = filepath.ToSlash(name) // fix for windows
		}
		if info.IsDir() {
			name := deepPath(realPath, info.Name())
			lr.Name = name
			lr.Path = filepath.Join(filepath.Dir(path), name)
			lr.Type = "dir"
			lr.Size = s.historyDirSize(lr.Path)
		} else {
			lr.Type = "file"
			lr.Size = info.Size() // formatSize(info)
		}
		lrs = append(lrs, lr)
	}

	for path, info := range dirInfoMap {
		if !auth.canAccess(info.Name()) {
			continue
		}
		_info, err := info.Info()
		if err != nil {
			log.Fatal("get dir info failed", err)
			continue
		}
		lr := HTTPFileInfo{
			Name:    _info.Name(),
			Path:    path,
			ModTime: _info.ModTime().UnixNano() / 1e6,
		}
		if search != "" {
			name, err := filepath.Rel(requestPath, path)
			if err != nil {
				log.Println(requestPath, path, err)
			}
			lr.Name = filepath.ToSlash(name) // fix for windows
		}
		if info.IsDir() {
			name := deepPath(realPath, info.Name())
			lr.Name = name
			lr.Path = filepath.Join(filepath.Dir(path), name)
			lr.Type = "dir"
			lr.Size = s.historyDirSize(lr.Path)
		} else {
			lr.Type = "file"
			lr.Size = _info.Size() // formatSize(info)
		}
		lrs = append(lrs, lr)
	}

	prefixReflects := make([]string, len(s.PrefixReflect))
	for i, re := range s.PrefixReflect {
		prefixReflects[i] = re.String()
	}

	data, _ := json.Marshal(map[string]interface{}{
		"files": lrs,
		"auth":  auth,
		"configs": ResponseConfigs{
			PrefixReflect: prefixReflects,
		},
	})
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

var dirInfoSize = Directory{size: make(map[string]int64), mutex: &sync.RWMutex{}}

func (s *HTTPStaticServer) makeIndex() error {
	var indexes = make([]IndexFileItem, 0)
	var err = filepath.Walk(s.Root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			log.Printf("WARN: Visit path: %s error: %v", strconv.Quote(path), err)
			return filepath.SkipDir
			// return err
		}
		if info.IsDir() {
			return nil
		}

		path, _ = filepath.Rel(s.Root, path)
		path = filepath.ToSlash(path)
		indexes = append(indexes, IndexFileItem{path, info})
		return nil
	})
	s.indexes = indexes
	return err
}

func (s *HTTPStaticServer) historyDirSize(dir string) int64 {
	dirInfoSize.mutex.RLock()
	size, ok := dirInfoSize.size[dir]
	dirInfoSize.mutex.RUnlock()

	if ok {
		return size
	}

	for _, fitem := range s.indexes {
		if strings.HasPrefix(fitem.Path, dir) {
			size += fitem.Info.Size()
		}
	}

	dirInfoSize.mutex.Lock()
	dirInfoSize.size[dir] = size
	dirInfoSize.mutex.Unlock()

	return size
}

func (s *HTTPStaticServer) findIndex(text string) []IndexFileItem {
	ret := make([]IndexFileItem, 0)
	for _, item := range s.indexes {
		ok := true
		// search algorithm, space for AND
		for _, keyword := range strings.Fields(text) {
			needContains := true
			if strings.HasPrefix(keyword, "-") {
				needContains = false
				keyword = keyword[1:]
			}
			if keyword == "" {
				continue
			}
			ok = (needContains == strings.Contains(strings.ToLower(item.Path), strings.ToLower(keyword)))
			if !ok {
				break
			}
		}
		if ok {
			ret = append(ret, item)
		}
	}
	return ret
}

func (s *HTTPStaticServer) defaultAccessConf() AccessConf {
	return AccessConf{
		Upload:   s.Upload,
		Delete:   s.Delete,
		Folder:   s.Folder,
		Download: s.Download,
	}
}

func (s *HTTPStaticServer) readAccessConf(realPath string) (ac AccessConf) {
	relativePath, err := filepath.Rel(s.Root, realPath)
	if err != nil || relativePath == "." || relativePath == "" { // actually relativePath is always "." if root == realPath
		ac = s.defaultAccessConf()
		realPath = s.Root
	} else {
		parentPath := filepath.Dir(realPath)
		ac = s.readAccessConf(parentPath)
	}
	if common.IsFile(realPath) {
		realPath = filepath.Dir(realPath)
	}
	cfgFile := filepath.Join(realPath, common.YAMLCONF)
	data, err := os.ReadFile(cfgFile)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		log.Printf("Err read .ghs.yml: %v", err)
	}
	err = yaml.Unmarshal(data, &ac)
	if err != nil {
		log.Printf("Err format .ghs.yml: %v", err)
	}
	return
}

func deepPath(basedir, name string) string {
	// loop max 5, incase of for loop not finished
	maxDepth := 5
	for depth := 0; depth <= maxDepth; depth += 1 {
		finfos, err := os.ReadDir(filepath.Join(basedir, name))
		if err != nil || len(finfos) != 1 {
			break
		}
		if finfos[0].IsDir() {
			name = filepath.ToSlash(filepath.Join(name, finfos[0].Name()))
		} else {
			break
		}
	}
	return name
}

func assetsContent(name string) string {
	fd, err := common.Assets.Open(name)
	if err != nil {
		panic(err)
	}
	data, err := io.ReadAll(fd)
	if err != nil {
		panic(err)
	}
	return string(data)
}

// TODO: I need to read more abouthtml/template
var (
	funcMap template.FuncMap
)

func init() {
	funcMap = template.FuncMap{
		"title": strings.ToTitle,
		"urlhash": func(path string) string {
			httpFile, err := common.Assets.Open(path)
			if err != nil {
				return path + "#no-such-file"
			}
			info, err := httpFile.Stat()
			if err != nil {
				return path + "#stat-error"
			}
			return fmt.Sprintf("%s?t=%d", path, info.ModTime().Unix())
		},
	}
}

var (
	_tmpls = make(map[string]*template.Template)
)

func renderHTML(w http.ResponseWriter, name string, v interface{}) {
	if t, ok := _tmpls[name]; ok {
		t.Execute(w, v)
		return
	}
	t := template.Must(template.New(name).Funcs(funcMap).Delims("[[", "]]").Parse(assetsContent(name)))
	_tmpls[name] = t
	t.Execute(w, v)
}

func checkFilename(name string) error {
	if strings.ContainsAny(name, "\\/:*<>|") {
		return errors.New("name should not contains \\/:*<>|")
	}
	return nil
}