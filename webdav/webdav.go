// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package webdav provides a WebDAV server implementation.
package webdav // import "golang.org/x/net/webdav"

import (
	"errors"
	"fmt"
	"go-aliyun-webdav/aliyun"
	"go-aliyun-webdav/aliyun/cache"
	"go-aliyun-webdav/aliyun/model"
	"io/ioutil"
	"reflect"
	"strconv"

	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"
)

type Handler struct {
	// Prefix is the URL path prefix to strip from WebDAV resource paths.
	Prefix string
	// FileSystem is the virtual file system.
	FileSystem FileSystem
	// LockSystem is the lock management system.
	LockSystem LockSystem
	// Logger is an optional error logger. If non-nil, it will be called
	// for all HTTP requests.
	Logger func(*http.Request, error)
	Config model.Config
}

func (h *Handler) stripPrefix(p string) (string, int, error) {
	if h.Prefix == "" {
		return p, http.StatusOK, nil
	}
	if r := strings.TrimPrefix(p, h.Prefix); len(r) < len(p) {
		return r, http.StatusOK, nil
	}
	return p, http.StatusNotFound, errPrefixMismatch
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	status, err := http.StatusBadRequest, errUnsupportedMethod
	if h.Config.ExpireTime < time.Now().Unix()-100 {
		refreshResult := aliyun.RefreshToken(h.Config.RefreshToken)
		config := model.Config{
			RefreshToken: refreshResult.RefreshToken,
			Token:        refreshResult.AccessToken,
			DriveId:      refreshResult.DefaultDriveId,
			ExpireTime:   time.Now().Unix() + refreshResult.ExpiresIn,
		}
		h.Config = config
	}
	switch r.Method {
	case "OPTIONS":
		status, err = h.handleOptions(w, r)
	case "GET", "HEAD", "POST":
		status, err = h.handleGetHeadPost(w, r)
	case "DELETE":
		status, err = h.handleDelete(w, r)
	case "PUT":
		status, err = h.handlePut(w, r)
	case "MKCOL":
		status, err = h.handleMkcol(w, r)
	case "COPY", "MOVE":
		status, err = h.handleCopyMove(w, r)
	case "LOCK":
		status, err = h.handleLock(w, r)
	case "UNLOCK":
		status, err = h.handleUnlock(w, r)
	case "PROPFIND":
		status, err = h.handlePropfind(w, r)
	case "PROPPATCH":
		status, err = h.handleProppatch(w, r)
	}

	if status != 0 {
		w.WriteHeader(status)
		if status != http.StatusNoContent {
			w.Write([]byte(StatusText(status)))
		}
	}
	if h.Logger != nil {
		h.Logger(r, err)
	}
}

func (h *Handler) lock(now time.Time, root string) (token string, status int, err error) {
	token, err = h.LockSystem.Create(now, LockDetails{
		Root:      root,
		Duration:  infiniteTimeout,
		ZeroDepth: true,
	})
	if err != nil {
		if err == ErrLocked {
			return "", StatusLocked, err
		}
		return "", http.StatusInternalServerError, err
	}
	return token, 0, nil
}

func (h *Handler) confirmLocks(r *http.Request, src, dst string) (release func(), status int, err error) {
	hdr := r.Header.Get("If")
	if hdr == "" {
		// An empty If header means that the client hasn't previously created locks.
		// Even if this client doesn't care about locks, we still need to check that
		// the resources aren't locked by another client, so we create temporary
		// locks that would conflict with another client's locks. These temporary
		// locks are unlocked at the end of the HTTP request.
		now, srcToken, dstToken := time.Now(), "", ""
		if src != "" {
			srcToken, status, err = h.lock(now, src)
			if err != nil {
				return nil, status, err
			}
		}
		if dst != "" {
			dstToken, status, err = h.lock(now, dst)
			if err != nil {
				if srcToken != "" {
					h.LockSystem.Unlock(now, srcToken)
				}
				return nil, status, err
			}
		}

		return func() {
			if dstToken != "" {
				h.LockSystem.Unlock(now, dstToken)
			}
			if srcToken != "" {
				h.LockSystem.Unlock(now, srcToken)
			}
		}, 0, nil
	}

	ih, ok := parseIfHeader(hdr)
	if !ok {
		return nil, http.StatusBadRequest, errInvalidIfHeader
	}
	// ih is a disjunction (OR) of ifLists, so any ifList will do.
	for _, l := range ih.lists {
		lsrc := l.resourceTag
		if lsrc == "" {
			lsrc = src
		} else {
			u, err := url.Parse(lsrc)
			if err != nil {
				continue
			}
			if u.Host != r.Host {
				continue
			}
			lsrc, status, err = h.stripPrefix(u.Path)
			if err != nil {
				return nil, status, err
			}
		}
		release, err = h.LockSystem.Confirm(time.Now(), lsrc, dst, l.conditions...)
		if err == ErrConfirmationFailed {
			continue
		}
		if err != nil {
			return nil, http.StatusInternalServerError, err
		}
		return release, 0, nil
	}
	// Section 10.4.1 says that "If this header is evaluated and all state lists
	// fail, then the request must fail with a 412 (Precondition Failed) status."
	// We follow the spec even though the cond_put_corrupt_token test case from
	// the litmus test warns on seeing a 412 instead of a 423 (Locked).
	return nil, http.StatusPreconditionFailed, ErrLocked
}

func (h *Handler) handleOptions(w http.ResponseWriter, r *http.Request) (status int, err error) {
	reqPath, status, err := h.stripPrefix(r.URL.Path)
	if err != nil {
		return status, err
	}
	ctx := r.Context()
	allow := "OPTIONS, LOCK, PUT, MKCOL"
	if fi, err := h.FileSystem.Stat(ctx, reqPath); err == nil {
		if fi.IsDir() {
			allow = "OPTIONS, LOCK, DELETE, PROPPATCH, COPY, MOVE, UNLOCK, PROPFIND"
		} else {
			allow = "OPTIONS, LOCK, GET, HEAD, POST, DELETE, PROPPATCH, COPY, MOVE, UNLOCK, PROPFIND, PUT"
		}
	}
	w.Header().Set("Allow", allow)
	// http://www.webdav.org/specs/rfc4918.html#dav.compliance.classes
	w.Header().Set("DAV", "1, 2")
	// http://msdn.microsoft.com/en-au/library/cc250217.aspx
	w.Header().Set("MS-Author-Via", "DAV")
	return 0, nil
}

func (h *Handler) handleGetHeadPost(w http.ResponseWriter, r *http.Request) (status int, err error) {
	//var data []byte
	var fi model.ListModel
	reqPath, status, err := h.stripPrefix(r.URL.Path)
	if len(reqPath) > 0 && !strings.HasSuffix(reqPath, "/") {
		strArr := strings.Split(reqPath, "/")

		fi = aliyun.GetFileDetail(h.Config.Token, h.Config.DriveId, getParentFileId(strArr))
		if fi.FileId == "" {
			fi, _, err = aliyun.Walk(h.Config.Token, h.Config.DriveId, strArr, getFileId(strArr))
			if err != nil {
				return 0, err
			}
		}
		if err != nil || fi.FileId == "" {
			return http.StatusNotFound, err
		}

		rangeStr := r.Header.Get("range")

		if len(rangeStr) > 0 && strings.LastIndex(rangeStr, "-") > 0 {
			rangeArr := strings.Split(rangeStr, "-")
			rangEnd, _ := strconv.ParseInt(rangeArr[1], 10, 64)
			if rangEnd >= fi.Size {
				rangeStr = rangeStr[:strings.LastIndex(rangeStr, "-")+1]
			}
		}
		//rangeStr = "bytes=0-" + strconv.Itoa(fi.Size)
		if r.Method != "HEAD" {
			if strings.Index(r.URL.String(), "025.jpg") > 0 {
			}
			downloadUrl := aliyun.GetDownloadUrl(h.Config.Token, h.Config.DriveId, fi.FileId)
			aliyun.GetFile(w, downloadUrl, h.Config.Token, rangeStr, r.Header.Get("if-range"))
		}

		if fi.Type == "folder" {
			return http.StatusMethodNotAllowed, nil
		}
		ctx := r.Context()
		etag, err := findETag(ctx, h.FileSystem, h.LockSystem, fi)
		if err != nil {
			return http.StatusInternalServerError, err
		}
		w.Header().Set("ETag", etag)
		return 0, nil

	}

	if err != nil {
		return status, err
	}
	// TODO: check locks for read-only access??

	if fi.Type == "folder" {
		return http.StatusMethodNotAllowed, nil
	}
	ctx1 := r.Context()
	etag, err := findETag(ctx1, h.FileSystem, h.LockSystem, fi)
	if err != nil {
		return http.StatusInternalServerError, err
	}
	w.Header().Set("ETag", etag)

	//http.ServeContent(w, r, reqPath, 0, fi.UpdatedAt)
	return 0, nil
}

func (h *Handler) handleDelete(w http.ResponseWriter, r *http.Request) (status int, err error) {
	reqPath, status, err := h.stripPrefix(r.URL.Path)
	if err != nil {
		return status, err
	}

	var fi model.ListModel
	if len(reqPath) > 0 {
		if strings.HasSuffix(reqPath, "/") {
			reqPath = reqPath[:len(reqPath)-1]
		}
		strArr := strings.Split(reqPath, "/")

		fi = aliyun.GetFileDetail(h.Config.Token, h.Config.DriveId, getParentFileId(strArr))
		if fi.Name == strArr[len(strArr)-1] {
			aliyun.RemoveTrash(h.Config.Token, h.Config.DriveId, fi.FileId, fi.ParentFileId)
			fmt.Println("🕺  删除", reqPath)
			cache.GoCache.Delete("FID_" + reqPath)
			cache.GoCache.Delete("FI_" + fi.FileId)
		} else {
			fi, _, walkerr := aliyun.Walk(h.Config.Token, h.Config.DriveId, strArr, "root")
			if walkerr == nil {
				if fi.Name == strArr[len(strArr)-1] {
					aliyun.RemoveTrash(h.Config.Token, h.Config.DriveId, fi.FileId, fi.ParentFileId)
					fmt.Println("🕺  删除", reqPath)
					cache.GoCache.Delete("FID_" + reqPath)
					cache.GoCache.Delete("FI_" + fi.FileId)
				}
			}
		}
	}

	return http.StatusNoContent, nil
}

func (h *Handler) handlePut(w http.ResponseWriter, r *http.Request) (status int, err error) {
	reqPath, status, err := h.stripPrefix(r.URL.Path)
	if _, ok := cache.GoCache.Get("IN_PROGRESS" + reqPath); ok {
		fmt.Println("❌ ❌ ❌  Already in progress", reqPath)
		return http.StatusCreated, errors.New("Upload in progress")
	}
	if err != nil {
		return status, err
	}
	if strings.Index(r.Header.Get("User-Agent"), "Darwin") > -1 && strings.Index(reqPath, "._") > -1 {
		return status, err
	}
	lastIndex := strings.LastIndex(reqPath, "/")
	fileName := reqPath[lastIndex+1:]
	if lastIndex == -1 {
		lastIndex = 0
		fileName = reqPath
	}
	var fi model.ListModel
	var walkerr error
	var parentFileId string
	if len(reqPath) > 0 && !strings.HasSuffix(reqPath, "/") {
		strArr2 := strings.Split(reqPath, "/")
		//如果父目录已经缓存，直接取
		if v, ok := cache.GoCache.Get("FID_" + strings.Join(strArr2[:len(strArr2)-1], "/")); ok {
			fi = aliyun.GetFileDetail(h.Config.Token, h.Config.DriveId, v.(string))
			parentFileId = fi.FileId
			fmt.Println("😊 😊  Cache hit", reqPath[:lastIndex])
		} else {
			//如果没找到缓存，尝试从root节点遍历，并设置缓存
			fmt.Println("😭 😭  Cache missing", reqPath[:lastIndex])
			strArr := strings.Split(reqPath[:lastIndex], "/")
			if len(strArr) == 1 {
				parentFileId = "root"
			}
			fi, _, walkerr = aliyun.WalkFolder(h.Config.Token, h.Config.DriveId, strArr, "", true)
			if walkerr == nil {
				if fi.Name != strArr[len(strArr)-1] {
					fmt.Println("🔥  Error: can't find parent folder", reqPath)
					return http.StatusConflict, errors.New("parent folder does not exist,please create first")
				} else {
					parentFileId = fi.FileId
					cache.GoCache.Set("FID_"+strings.Join(strArr, "/"), fi.FileId, -1)
					cache.GoCache.Set("FI_"+fi.FileId, fi, -1)
					fmt.Println("😊 😊  Cache set", strings.Join(strArr, "/"))
				}
			} else {
				fmt.Println("🔥  Error: can't find parent folder", reqPath, walkerr)
				return http.StatusConflict, errors.New("parent folder does not exist,please create first")
			}
		}
	}

	//if r.ContentLength == 0 {
	//	return http.StatusCreated, nil
	//}
	defer func(rp string) {
		if _, ok := cache.GoCache.Get("IN_PROGRESS" + rp); ok {
			cache.GoCache.Delete("IN_PROGRESS" + rp)
		}
	}(reqPath)
	cache.GoCache.Set("IN_PROGRESS"+reqPath, fileName, -1)
	fileId := aliyun.ContentHandle(r, h.Config.Token, h.Config.DriveId, parentFileId, fileName)
	cache.GoCache.Delete("IN_PROGRESS" + reqPath)
	if fileId != "" {
		cache.GoCache.Set("FID_"+reqPath, fileId, -1)
	} else {
		fmt.Println("❌  Upload failed", reqPath)
		return http.StatusBadRequest, errors.New("Upload failed")
	}
	return http.StatusCreated, nil
}

func (h *Handler) handleMkcol(w http.ResponseWriter, r *http.Request) (status int, err error) {
	reqPath, status, err := h.stripPrefix(r.URL.Path)
	if strings.HasSuffix(reqPath, "/") {
		reqPath = reqPath[:len(reqPath)-1]
	}
	if err != nil {
		return status, err
	}

	if r.ContentLength > 0 {
		return http.StatusUnsupportedMediaType, nil
	}

	if len(reqPath) > 0 {
		parentFileId := "root"
		var name string = reqPath
		//var fi model.ListModel
		index := strings.LastIndex(reqPath, "/")
		//2层以上
		if index > -1 {
			strArr := strings.Split(reqPath, "/")
			//try to get parent folder detail
			pi := aliyun.GetFileDetail(h.Config.Token, h.Config.DriveId, getFileId(strArr))
			if reflect.DeepEqual(pi, model.ListModel{}) || pi.FileId == "root" {
				p, chd, walkerr := aliyun.WalkFolder(h.Config.Token, h.Config.DriveId, strArr[:len(strArr)-1], "", true)
				if walkerr != nil {
					fmt.Println("❌ ❌  parent folder not found, Request path", reqPath)
					return http.StatusConflict, errors.New("parent folder not found")
				} else {
					fmt.Println("-----Found parent", p.Name, "Requested", strArr[:len(strArr)-1])
				}
				pi = p
				for _, item := range chd.Items {
					if len(strArr) == 2 {
						cache.GoCache.Set("FID_"+strArr[0]+"/"+item.Name, item.FileId, -1)
					} else {
						cache.GoCache.Set("FID_"+strings.Join(strArr[:len(strArr)-1], "/")+"/"+item.Name, item.FileId, -1)
					}
					cache.GoCache.Set("FI_"+item.FileId, item, -1)
					if item.Name == strArr[len(strArr)-1] {
						fmt.Println("❌ ❌  folder already exists, Request path", reqPath)
						return http.StatusContinue, errors.New("folder already exists")
					}
				}
				if len(strArr) == 2 {
					cache.GoCache.Set("FID_"+strArr[0], p.FileId, -1)
				} else {
					cache.GoCache.Set("FID_"+strings.Join(strArr[:len(strArr)-1], "/"), p.FileId, -1)
				}

				cache.GoCache.Set("FI_"+p.FileId, p, -1)
			} else {
				fmt.Println("-----Found parent", pi.Name, "Requested", strArr[:len(strArr)-1])
			}
			fmt.Println(pi)
			parentFileId = pi.FileId
			name = reqPath[index+1:]
		}
		fmt.Println("📁  Creating Directory", reqPath, parentFileId)
		dir := aliyun.MakeDir(h.Config.Token, h.Config.DriveId, name, parentFileId)
		if (dir != model.ListModel{}) {
			cache.GoCache.Set("FID_"+reqPath, dir.FileId, -1)
			cache.GoCache.Set("FI"+dir.FileId, dir, -1)
			if va, ok := cache.GoCache.Get(parentFileId); ok {
				l := va.(model.FileListModel)
				l.Items = append(l.Items, aliyun.GetFileDetail(h.Config.Token, h.Config.DriveId, dir.FileId))
				cache.GoCache.SetDefault(parentFileId, l)
			}
			fmt.Println("✅  Directory created", reqPath, dir.ParentFileId, parentFileId)
		} else {
			fmt.Println("❌  Create Directory Failed", reqPath)
			return http.StatusConflict, errors.New("create directory failed: " + reqPath)
		}
	}

	return http.StatusCreated, nil
}

func (h *Handler) handleCopyMove(w http.ResponseWriter, r *http.Request) (status int, err error) {
	hdr := r.Header.Get("Destination")
	if hdr == "" {
		return http.StatusBadRequest, errInvalidDestination
	}
	u, err := url.Parse(hdr)
	if err != nil {
		return http.StatusBadRequest, errInvalidDestination
	}
	if u.Host != "" && u.Host != r.Host {
		return http.StatusBadGateway, errInvalidDestination
	}

	src, status, err := h.stripPrefix(r.URL.Path)
	if err != nil {
		return status, err
	}
	src = strings.TrimRight(src, "/")
	src = strings.TrimLeft(src, "/")

	dst, status, err := h.stripPrefix(u.Path)
	if err != nil {
		return status, err
	}
	dst = strings.TrimRight(dst, "/")
	dst = strings.TrimLeft(dst, "/")

	if dst == "" {
		return http.StatusBadGateway, errInvalidDestination
	}

	srcIndex := strings.LastIndex(src, "/")
	//if runtime.GOOS == "darwin" {
	//	dstIndex = len(dst)
	//} else {
	//	dstIndex = strings.LastIndex(dst, "/")
	//}
	dstIndex := strings.LastIndex(dst, "/")

	rename := false
	if srcIndex == -1 && srcIndex == dstIndex {
		rename = true
	} else {
		if srcIndex == dstIndex {
			srcPrefix := src[:srcIndex+1]
			dstPrefix := dst[:dstIndex+1]
			if srcPrefix == dstPrefix {
				rename = true
			}
		}
	}

	if rename {
		var fi model.ListModel
		strArr := strings.Split(src, "/")
		fi = aliyun.GetFileDetail(h.Config.Token, h.Config.DriveId, getParentFileId(strArr))
		if fi.FileId == "" {
			fi, _, err = aliyun.Walk(h.Config.Token, h.Config.DriveId, strArr, getFileId(strArr))
			if err != nil {
				return 0, err
			}
		}
		if err != nil || fi.FileId == "" {
			return http.StatusNotFound, err
		}

		if dstIndex == -1 {
			dstIndex = 0
		} else {
			dstIndex += 1
		}
		aliyun.ReName(h.Config.Token, h.Config.DriveId, dst[dstIndex:], fi.FileId)
		return http.StatusNoContent, nil
	}

	if src[srcIndex+1:] == dst[dstIndex+1:] && srcIndex != dstIndex {
		var fi model.ListModel
		strArr := strings.Split(src, "/")
		fi = aliyun.GetFileDetail(h.Config.Token, h.Config.DriveId, getParentFileId(strArr))
		if fi.FileId == "" {
			fi, _, err = aliyun.Walk(h.Config.Token, h.Config.DriveId, strArr, getFileId(strArr))
			if err != nil {
				return http.StatusNotFound, err
			}
		}
		if err != nil || fi.FileId == "" {
			return http.StatusNotFound, err
		}

		strArrParent := strings.Split(dst[:dstIndex], "/")
		parent := aliyun.GetFileDetail(h.Config.Token, h.Config.DriveId, getParentFileId(strArrParent))
		if parent.FileId == "" {
			parent, _, err = aliyun.Walk(h.Config.Token, h.Config.DriveId, strArrParent, getFileId(strArrParent))
			if err != nil {
				return http.StatusNotFound, err
			}
		}
		if err != nil || parent.FileId == "" {
			return http.StatusNotFound, err
		}
		aliyun.BatchFile(h.Config.Token, h.Config.DriveId, fi.FileId, parent.FileId)
		cache.GoCache.Delete("FID_" + src)
		cache.GoCache.Delete("FID_" + src[:srcIndex])
		cache.GoCache.Delete("FID_" + dst)
		cache.GoCache.Delete("FID_" + dst[:dstIndex])
		return http.StatusNoContent, nil
	}

	ctx := r.Context()

	if r.Method == "MOVE" {
		//fmt.Println("move")
	}

	if r.Method == "COPY" {
		// Section 7.5.1 says that a COPY only needs to lock the destination,
		// not both destination and source. Strictly speaking, this is racy,
		// even though a COPY doesn't modify the source, if a concurrent
		// operation modifies the source. However, the litmus test explicitly
		// checks that COPYing a locked-by-another source is OK.
		release, status, err := h.confirmLocks(r, "", dst)
		if err != nil {
			return status, err
		}
		defer release()

		// Section 9.8.3 says that "The COPY method on a collection without a Depth
		// header must act as if a Depth header with value "infinity" was included".
		depth := infiniteDepth
		if hdr := r.Header.Get("Depth"); hdr != "" {
			depth = parseDepth(hdr)
			if depth != 0 && depth != infiniteDepth {
				// Section 9.8.3 says that "A client may submit a Depth header on a
				// COPY on a collection with a value of "0" or "infinity"."
				return http.StatusBadRequest, errInvalidDepth
			}
		}
		return copyFiles(ctx, h.FileSystem, src, dst, r.Header.Get("Overwrite") != "F", depth, 0)
	}

	//release, status, err := h.confirmLocks(r, src, dst)
	//if err != nil {
	//	return status, err
	//}
	//defer release()

	// Section 9.9.2 says that "The MOVE method on a collection must act as if
	// a "Depth: infinity" header was used on it. A client must not submit a
	// Depth header on a MOVE on a collection with any value but "infinity"."
	if hdr := r.Header.Get("Depth"); hdr != "" {
		if parseDepth(hdr) != infiniteDepth {
			return http.StatusBadRequest, errInvalidDepth
		}
	}
	return moveFiles(ctx, h.FileSystem, src, dst, r.Header.Get("Overwrite") == "T")
}

func (h *Handler) handleLock(w http.ResponseWriter, r *http.Request) (retStatus int, retErr error) {
	userAgent := r.Header.Get("User-Agent")
	if len(userAgent) > 0 && strings.Index(userAgent, "Darwin") > -1 {
		//_macLockRequest = true
		//
		//String
		//timeString = new
		//Long(System.currentTimeMillis())
		//.toString()
		//_lockOwner = _userAgent.concat(timeString)
		userAgent += strconv.FormatInt(time.Now().UnixNano()/1000000, 10)
	}
	duration, err := parseTimeout(r.Header.Get("Timeout"))
	if err != nil {
		return http.StatusBadRequest, err
	}
	li, status, err := readLockInfo(r.Body)
	if err != nil {
		return status, err
	}

	//ctx := r.Context()
	token, ld, now, created := "", LockDetails{}, time.Now(), false
	if li == (lockInfo{}) {
		// An empty lockInfo means to refresh the lock.
		ih, ok := parseIfHeader(r.Header.Get("If"))
		if !ok {
			return http.StatusBadRequest, errInvalidIfHeader
		}
		if len(ih.lists) == 1 && len(ih.lists[0].conditions) == 1 {
			token = ih.lists[0].conditions[0].Token
		}
		if token == "" {
			return http.StatusBadRequest, errInvalidLockToken
		}
		ld, err = h.LockSystem.Refresh(now, token, duration)
		if err != nil {
			if err == ErrNoSuchLock {
				return http.StatusPreconditionFailed, err
			}
			return http.StatusInternalServerError, err
		}

	} else {
		// Section 9.10.3 says that "If no Depth header is submitted on a LOCK request,
		// then the request MUST act as if a "Depth:infinity" had been submitted."
		depth := infiniteDepth
		if hdr := r.Header.Get("Depth"); hdr != "" {
			depth = parseDepth(hdr)
			if depth != 0 && depth != infiniteDepth {
				// Section 9.10.3 says that "Values other than 0 or infinity must not be
				// used with the Depth header on a LOCK method".
				return http.StatusBadRequest, errInvalidDepth
			}
		}
		reqPath, status, err := h.stripPrefix(r.URL.Path)
		if err != nil {
			return status, err
		}
		ld = LockDetails{
			Root:      reqPath,
			Duration:  duration,
			OwnerXML:  li.Owner.InnerXML,
			ZeroDepth: depth == 0,
		}
		token, err = h.LockSystem.Create(now, ld)
		if err != nil {
			if err == ErrLocked {
				return StatusLocked, err
			}
			return http.StatusInternalServerError, err
		}
		defer func() {
			if retErr != nil {
				h.LockSystem.Unlock(now, token)
			}
		}()

		// Create the resource if it didn't previously exist.
		var fi model.ListModel
		lastIndex := strings.LastIndex(reqPath, "/")
		if lastIndex == -1 {
			lastIndex = 0
		}
		if len(reqPath) > 0 && !strings.HasSuffix(reqPath, "/") {
			strArr := strings.Split(reqPath, "/")
			parent := aliyun.GetFileDetail(h.Config.Token, h.Config.DriveId, getFileId(strArr))
			if reflect.DeepEqual(parent, model.ListModel{}) || (len(strArr) > 2 && parent.Name != strArr[len(strArr)-2]) {
				parent, _, _ = aliyun.Walk(h.Config.Token, h.Config.DriveId, strArr[:len(strArr)-1], getFileId(strArr))
				fmt.Println("❌  父目录不存在", reqPath)
				return http.StatusConflict, errors.New("parent folder doesn't exist")
			}
			fi = aliyun.GetFileDetail(h.Config.Token, h.Config.DriveId, getParentFileId(strArr))
		}
		if reflect.DeepEqual(fi, model.ListModel{}) {
			created = true
		}

		// http://www.webdav.org/specs/rfc4918.html#HEADER_Lock-Token says that the
		// Lock-Token value is a Coded-URL. We add angle brackets.
		w.Header().Set("Lock-Token", "<"+token+">")
	}

	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	if created {
		// This is "w.WriteHeader(http.StatusCreated)" and not "return
		// http.StatusCreated, nil" because we write our own (XML) response to w
		// and Handler.ServeHTTP would otherwise write "Created".
		w.WriteHeader(http.StatusCreated)
	}
	writeLockInfo(w, token, ld)
	return 0, nil
}

func (h *Handler) handleUnlock(w http.ResponseWriter, r *http.Request) (status int, err error) {
	// http://www.webdav.org/specs/rfc4918.html#HEADER_Lock-Token says that the
	// Lock-Token value is a Coded-URL. We strip its angle brackets.
	t := r.Header.Get("Lock-Token")
	if len(t) < 2 || t[0] != '<' || t[len(t)-1] != '>' {
		return http.StatusBadRequest, errInvalidLockToken
	}
	t = t[1 : len(t)-1]

	switch err = h.LockSystem.Unlock(time.Now(), t); err {
	case nil:
		return http.StatusNoContent, err
	case ErrForbidden:
		return http.StatusForbidden, err
	case ErrLocked:
		return StatusLocked, err
	case ErrNoSuchLock:
		return http.StatusConflict, err
	default:
		return http.StatusInternalServerError, err
	}
}

func (h *Handler) handlePropfind(w http.ResponseWriter, r *http.Request) (status int, err error) {
	if r.ContentLength > 0 {
		available, _ := ioutil.ReadAll(r.Body)
		if strings.Contains(string(available), "quota-available-bytes") {
			totle, used := aliyun.GetBoxSize(h.Config.Token)
			to, _ := strconv.ParseInt(string(totle), 10, 64)
			us, _ := strconv.ParseInt(string(used), 10, 64)
			w.Write([]byte(`<?xml version="1.0" encoding="utf-8"?><D:multistatus xmlns:D="DAV:"><D:response><D:href>/</D:href><D:propstat><D:prop><D:quota-available-bytes>` + strconv.FormatInt(to-us, 10) + `</D:quota-available-bytes><D:quota-used-bytes>` + used + `</D:quota-used-bytes></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>
			</D:multistatus>`))
			return 0, nil
		}
		//fmt.Println(string(available))
	}
	reqPath, status, err := h.stripPrefix(r.URL.Path)
	var list model.FileListModel
	var fi model.ListModel
	//fmt.Println(reqPath)
	var unfindListErr error
	var walkErr error
	//定位当前文件或文件夹位置,假设同级目录下无重名文件或文件夹
	if strings.HasSuffix(reqPath, "/") {
		reqPath = reqPath[0 : len(reqPath)-1]
	}
	var parentFileId string
	if reqPath == "" {
		parentFileId = "root"
	} else {
		paths := strings.Split(reqPath, "/")
		if len(paths) == 1 {
			parentFileId = "root"
		} else {
			if pid, err := cache.GoCache.Get("FID_" + strings.Join(paths[:len(paths)-1], "/")); err {
				parentFileId = pid.(string)
			} else {
				parentFileId = "root"
			}
		}
	}

	fi, list, walkErr = aliyun.Walk(h.Config.Token, h.Config.DriveId, strings.Split(reqPath, "/"), parentFileId)
	if walkErr == nil && fi.FileId != "" {
		cache.GoCache.Set("FID_"+reqPath, fi.FileId, -1)
		for _, i := range list.Items {
			cache.GoCache.Set("FID_"+reqPath+"/"+i.Name, i.FileId, -1)
		}
	}

	if walkErr != nil {
		return http.StatusNotFound, errors.New("not exists")
	}
	ctx := r.Context()
	if (walkErr != nil || fi == model.ListModel{}) && reqPath != "" && reqPath != "/" && strings.Index(reqPath, "test.png") == -1 {
		//新建或修改名称的时候需要判断是否已存在
		if len(list.Items) == 0 || unfindListErr != nil {
			return http.StatusNotFound, walkErr
		}

		//return http.StatusMethodNotAllowed, err
	}
	depth := infiniteDepth
	if hdr := r.Header.Get("Depth"); hdr != "" {
		depth = parseDepth(hdr)
		if depth == invalidDepth {
			return http.StatusBadRequest, errInvalidDepth
		}
	}
	pf, status, err := readPropfind(r.Body)
	if err != nil {
		return status, err
	}

	mw := multistatusWriter{w: w}

	walkFn := func(parent model.ListModel, info model.FileListModel, err error) error {
		if reflect.DeepEqual(parent, model.ListModel{}) {
			parent.Type = "folder"
			parent.ParentFileId = "root"
		}
		if err != nil {
			return err
		}
		var pstats []Propstat
		if pf.Propname != nil {
			pnames, err := propnames(parent)
			if err != nil {
				return err
			}
			pstat := Propstat{Status: http.StatusOK}
			for _, xmlname := range pnames {
				pstat.Props = append(pstat.Props, Property{XMLName: xmlname})
			}
			pstats = append(pstats, pstat)
		} else if pf.Allprop != nil {
			pstats, err = allprop(ctx, h.FileSystem, h.LockSystem, pf.Prop, parent)
		} else {
			pstats, err = props(ctx, h.FileSystem, h.LockSystem, pf.Prop, parent)
		}
		if err != nil {
			return err
		}
		href := path.Join(h.Prefix, parent.Name)
		if parent.ParentFileId == "root" && parent.FileId == "" {
			href = "/" + parent.Name
		} else {
			href, _ = aliyun.GetFilePath(h.Config.Token, h.Config.DriveId, parent.ParentFileId, parent.FileId, parent.Type)
			href += parent.Name
			if parent.Type == "folder" {
				href += "/"
			}
			//list, _ = aliyun.GetList(h.Config.Token, h.Config.DriveId, parent.FileId)

		}
		return mw.write(makePropstatResponse(href, pstats))
	}
	userAgent := r.Header.Get("User-Agent")
	cheng := 1
	walkError := walkFS(ctx, h.FileSystem, depth, fi, list, walkFn, h.Config.Token, h.Config.DriveId, userAgent, cheng)
	closeErr := mw.close()
	if walkError != nil {
		return http.StatusInternalServerError, walkErr
	}
	if closeErr != nil {
		return http.StatusInternalServerError, closeErr
	}
	return 0, nil
}

func getParentFileId(strArr []string) string {
	cacheKey := strArr[0]
	for _, folder := range strArr[1:] {
		cacheKey = cacheKey + "/" + folder
	}
	va, ok := cache.GoCache.Get("FID_" + cacheKey)
	if ok {
		return va.(string)
	} else {
		return "root"
	}

}

func getFileId(strArr []string) string {
	//如果是新建或者修改文件或者文件夹，获取上级的parentFileId
	if len(strArr) > 1 {
		cacheKey := strArr[0]
		for _, folder := range strArr[1 : len(strArr)-1] {
			cacheKey = cacheKey + "/" + folder
		}
		va, ok := cache.GoCache.Get("FID_" + cacheKey)
		if ok {
			return va.(string)
		} else {
			return "root"
		}

	} else {
		va, ok := cache.GoCache.Get("FID_" + strArr[0])
		if ok {
			return va.(string)
		} else {
			return "root"
		}
	}
}

func (h *Handler) handleProppatch(w http.ResponseWriter, r *http.Request) (status int, err error) {
	reqPath, status, err := h.stripPrefix(r.URL.Path)
	if err != nil {
		return status, err
	}
	release, status, err := h.confirmLocks(r, reqPath, "")
	if err != nil {
		return status, err
	}
	defer release()

	ctx := r.Context()

	if _, err := h.FileSystem.Stat(ctx, reqPath); err != nil {
		if os.IsNotExist(err) {
			return http.StatusNotFound, err
		}
		return http.StatusMethodNotAllowed, err
	}
	patches, status, err := readProppatch(r.Body)
	if err != nil {
		return status, err
	}
	pstats, err := patch(ctx, h.FileSystem, h.LockSystem, reqPath, patches)
	if err != nil {
		return http.StatusInternalServerError, err
	}
	mw := multistatusWriter{w: w}
	writeErr := mw.write(makePropstatResponse(r.URL.Path, pstats))
	closeErr := mw.close()
	if writeErr != nil {
		return http.StatusInternalServerError, writeErr
	}
	if closeErr != nil {
		return http.StatusInternalServerError, closeErr
	}
	return 0, nil
}

func makePropstatResponse(href string, pstats []Propstat) *response {
	resp := response{
		Href:     []string{(&url.URL{Path: href}).EscapedPath()},
		Propstat: make([]propstat, 0, len(pstats)),
	}
	for _, p := range pstats {
		var xmlErr *xmlError
		if p.XMLError != "" {
			xmlErr = &xmlError{InnerXML: []byte(p.XMLError)}
		}
		resp.Propstat = append(resp.Propstat, propstat{
			Status:              fmt.Sprintf("HTTP/1.1 %d %s", p.Status, StatusText(p.Status)),
			Prop:                p.Props,
			ResponseDescription: p.ResponseDescription,
			Error:               xmlErr,
		})
	}
	return &resp
}

const (
	infiniteDepth = -1
	invalidDepth  = -2
)

// parseDepth maps the strings "0", "1" and "infinity" to 0, 1 and
// infiniteDepth. Parsing any other string returns invalidDepth.
//
// Different WebDAV methods have further constraints on valid depths:
//	- PROPFIND has no further restrictions, as per section 9.1.
//	- COPY accepts only "0" or "infinity", as per section 9.8.3.
//	- MOVE accepts only "infinity", as per section 9.9.2.
//	- LOCK accepts only "0" or "infinity", as per section 9.10.3.
// These constraints are enforced by the handleXxx methods.
func parseDepth(s string) int {
	switch s {
	case "0":
		return 0
	case "1":
		return 1
	case "infinity":
		return infiniteDepth
	}
	return invalidDepth
}

// http://www.webdav.org/specs/rfc4918.html#status.code.extensions.to.http11
const (
	StatusMulti               = 207
	StatusUnprocessableEntity = 422
	StatusLocked              = 423
	StatusFailedDependency    = 424
	StatusInsufficientStorage = 507
)

func StatusText(code int) string {
	switch code {
	case StatusMulti:
		return "Multi-Status"
	case StatusUnprocessableEntity:
		return "Unprocessable Entity"
	case StatusLocked:
		return "Locked"
	case StatusFailedDependency:
		return "Failed Dependency"
	case StatusInsufficientStorage:
		return "Insufficient Storage"
	}
	return http.StatusText(code)
}

var (
	errDestinationEqualsSource = errors.New("webdav: destination equals source")
	errDirectoryNotEmpty       = errors.New("webdav: directory not empty")
	errInvalidDepth            = errors.New("webdav: invalid depth")
	errInvalidDestination      = errors.New("webdav: invalid destination")
	errInvalidIfHeader         = errors.New("webdav: invalid If header")
	errInvalidLockInfo         = errors.New("webdav: invalid lock info")
	errInvalidLockToken        = errors.New("webdav: invalid lock token")
	errInvalidPropfind         = errors.New("webdav: invalid propfind")
	errInvalidProppatch        = errors.New("webdav: invalid proppatch")
	errInvalidResponse         = errors.New("webdav: invalid response")
	errInvalidTimeout          = errors.New("webdav: invalid timeout")
	errNoFileSystem            = errors.New("webdav: no file system")
	errNoLockSystem            = errors.New("webdav: no lock system")
	errNotADirectory           = errors.New("webdav: not a directory")
	errPrefixMismatch          = errors.New("webdav: prefix mismatch")
	errRecursionTooDeep        = errors.New("webdav: recursion too deep")
	errUnsupportedLockInfo     = errors.New("webdav: unsupported lock info")
	errUnsupportedMethod       = errors.New("webdav: unsupported method")
)
