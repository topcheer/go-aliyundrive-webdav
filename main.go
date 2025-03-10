package main

import (
	"context"
	"flag"
	"fmt"
	"go-aliyun-webdav/aliyun"
	"go-aliyun-webdav/aliyun/cache"
	"go-aliyun-webdav/aliyun/model"
	"go-aliyun-webdav/webdav"
	"reflect"

	//"gorm.io/driver/sqlite"
	//"gorm.io/gorm"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"
)

func init() {
	cache.Init()
}

var Version = "v1.1.1"

type Task struct {
	Id string `json:"id"`
}

func main() {
	//GetDb()
	var port *string
	var path *string
	var refreshToken *string
	var user *string
	var pwd *string
	var versin *bool
	var log *bool
	var check *string

	//
	port = flag.String("port", "8085", "默认8085")
	path = flag.String("path", "./", "")
	user = flag.String("user", "admin", "用户名")
	pwd = flag.String("pwd", "123456", "密码")
	versin = flag.Bool("V", false, "显示版本")
	log = flag.Bool("v", false, "是否显示日志(默认不显示)")
	//log = flag.Bool("v", true, "是否显示日志(默认不显示)")
	refreshToken = flag.String("rt", "", "refresh_token")

	check = flag.String("crt", "", "检查refreshToken是否过期")

	flag.Parse()
	if *versin {
		fmt.Println(Version)
		return
	}

	if len(*check) > 0 {
		refreshResult := aliyun.RefreshToken(*check)
		if reflect.DeepEqual(refreshResult, model.RefreshTokenModel{}) {

			fmt.Println("refreshToken已过期")
		} else {
			fmt.Println("refreshToken可以使用")
		}
		return
	}

	if len(*refreshToken) == 0 {
		fmt.Println("rt为必填项,请输入refreshToken")
		return
	}
	if len(os.Args) > 2 && os.Args[1] == "rt" {
		*refreshToken = os.Args[2]
	}
	var address string
	if runtime.GOOS == "windows" {
		address = ":" + *port
	} else {

		address = "0.0.0.0:" + *port
	}
	refreshResult := aliyun.RefreshToken(*refreshToken)
	if reflect.DeepEqual(refreshResult, model.RefreshTokenModel{}) {
		fmt.Println("refreshToken已过期")
		return
	} else {
		fmt.Println("refreshToken可以使用")
	}

	config := model.Config{
		RefreshToken: refreshResult.RefreshToken,
		Token:        refreshResult.AccessToken,
		DriveId:      refreshResult.DefaultDriveId,
		ExpireTime:   time.Now().Unix() + refreshResult.ExpiresIn,
	}

	fs := &webdav.Handler{
		Prefix:     "/",
		FileSystem: webdav.Dir(*path),
		LockSystem: webdav.NewMemLS(),
		Config:     config,
	}

	//fmt.p

	http.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		// 获取用户名/密码
		username, password, ok := req.BasicAuth()
		if !ok {
			w.Header().Set("WWW-Authenticate", `Basic realm="Restricted"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		//	 验证用户名/密码
		if username != *user || password != *pwd {
			http.Error(w, "WebDAV: need authorized!", http.StatusUnauthorized)
			return
		}

		// Add CORS headers before any operation so even on a 401 unauthorized status, CORS will work.

		w.Header().Set("Access-Control-Allow-Origin", "*")

		w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS, PUT, DELETE,UPDATE")

		w.Header().Set("Access-Control-Allow-Credentials", "true")

		if req.Method == "GET" && strings.HasPrefix(req.URL.Path, fs.Prefix) {
			info, err := fs.FileSystem.Stat(context.TODO(), strings.TrimPrefix(req.URL.Path, fs.Prefix))
			if err == nil && info.IsDir() {
				req.Method = "PROPFIND"

				if req.Header.Get("Depth") == "" {
					req.Header.Add("Depth", "1")
				}
			}
		}
		if *log {
			fmt.Println(req.URL)
			fmt.Println(req.Method)
		}
		fs.ServeHTTP(w, req)
	})
	go refresh(fs)
	http.ListenAndServe(address, nil)

}
func refresh(fs *webdav.Handler) {
	//每隔10小时刷新一下RefreshToken
	timer := time.NewTimer(10 * time.Hour)
	for {
		timer.Reset(10 * time.Hour)
		select {
		case <-timer.C:
			refreshResult := aliyun.RefreshToken(fs.Config.RefreshToken)
			fs.Config = model.Config{
				RefreshToken: refreshResult.RefreshToken,
				Token:        refreshResult.AccessToken,
				DriveId:      refreshResult.DefaultDriveId,
				ExpireTime:   time.Now().Unix() + refreshResult.ExpiresIn,
			}
		}
	}
}
