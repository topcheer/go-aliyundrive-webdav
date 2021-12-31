package aliyun

import (
	"crypto/md5"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"go-aliyun-webdav/aliyun/cache"
	"go-aliyun-webdav/aliyun/model"
	"go-aliyun-webdav/aliyun/net"
	"go-aliyun-webdav/utils"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

//处理内容
func ContentHandle(r *http.Request, token string, driveId string, parentId string, fileName string) string {
	//需要判断参数里面的有效期
	//默认截取长度10485760
	//const DEFAULT int64 = 10485760
	const DEFAULT int64 = 10485760
	var count float64 = 1

	if len(parentId) == 0 {
		parentId = "root"
	}
	if r.ContentLength > 0 {
		count = math.Ceil(float64(r.ContentLength) / float64(DEFAULT))
	} else {
		//空文件处理
		sha1_0 := "DA39A3EE5E6B4B0D3255BFEF95601890AFD80709"
		_, _, fileId, _ := UpdateFileFile(token, driveId, fileName, parentId, "0", 1, sha1_0, "", true)
		if fileId != "" {
			fmt.Println("0⃣️  Created zero byte file", r.URL.Path)
			if va, ok := cache.GoCache.Get(parentId); ok {
				l := va.(model.FileListModel)
				l.Items = append(l.Items, GetFileDetail(token, driveId, fileId))
				cache.GoCache.SetDefault(parentId, l)
			}

			return fileId
		} else {
			fmt.Println("❌  Unable to create zero byte file", r.URL.Path)
			return ""
		}
	}

	//proof 偏移量
	var offset int64 = 0
	//proof内容base64
	var proof string = ""
	//是否闪传
	var flashUpload bool = false
	//status code
	var code int
	var uploadUrl []gjson.Result
	var uploadId string
	var uploadFileId string
	var uid = uuid.New().String()
	var intermediateFile, err = os.Create(uid)
	if err != nil {
		fmt.Println("❌ ❌ ❌  Error Creating Intermediate File", r.URL.Path)
		return ""
	}
	defer func(create *os.File) {
		err := create.Close()
		if err != nil {
			fmt.Println(err)
		}
	}(intermediateFile)
	defer func(name string) {
		err := os.Remove(name)
		if err != nil {
			fmt.Println(err, name)
		}
	}(intermediateFile.Name())
	//写入中间文件
	_, copyError := io.Copy(intermediateFile, r.Body)
	if copyError != nil {
		fmt.Println("❌  Error creating intermediate file ", intermediateFile.Name(), r.ContentLength)
		return ""
	}
	//大于150K小于25G的才开启闪传
	//由于webdav协议的局限性，使用中间文件，服务求要有足够的存储，否则会将硬盘撑爆掉
	if r.ContentLength > 1024*150 && r.ContentLength <= 1024*1024*1024*25 {
		preHashDataBytes := make([]byte, 1024)
		_, err := intermediateFile.ReadAt(preHashDataBytes, 0)
		if err != nil {
			fmt.Println("❌  error reading file", intermediateFile.Name(), err, r.URL.Path)
			return ""
		}
		h := sha1.New()
		h.Write(preHashDataBytes)
		//检查是否可以极速上传，逻辑如下
		//取文件的前1K字节，做SHA1摘要，调用创建文件接口，pre_hash参数为SHA1摘要，如果返回409，则这个文件可以极速上传
		preHashRequest := `{"drive_id":"` + driveId + `","parent_file_id":"` + parentId + `","name":"` + fileName + `","type":"file","check_name_mode":"overwrite","size":` + strconv.FormatInt(r.ContentLength, 10) + `,"pre_hash":"` + hex.EncodeToString(h.Sum(nil)) + `","proof_version":"v1"}`
		_, code = net.PostExpectStatus(model.APIFILEUPLOAD, token, []byte(preHashRequest))
		if code == 409 {
			md := md5.New()
			tokenBytes := []byte(token)
			md.Write(tokenBytes)
			tokenMd5 := hex.EncodeToString(md.Sum(nil))
			first16 := tokenMd5[:16]
			f, err := strconv.ParseUint(first16, 16, 64)
			if err != nil {
				fmt.Println(err)
			}
			offset = int64(f % uint64(r.ContentLength))
			end := math.Min(float64(offset+8), float64(r.ContentLength))
			off := make([]byte, int64(end)-offset)
			_, errS := intermediateFile.Seek(0, 0)
			if errS != nil {
				fmt.Println("❌  error seek file", intermediateFile.Name(), err, r.URL.Path)
				return ""
			}
			_, offerr := intermediateFile.ReadAt(off, offset)
			if offerr != nil {
				fmt.Println("❌  Can't calculate proof", offerr, r.URL.Path)
				return ""
			}
			proof = utils.GetProof(off)
			flashUpload = true
		}
		_, seekError := intermediateFile.Seek(0, 0)
		if seekError != nil {
			fmt.Println("❌  seek error ", seekError, r.URL.Path, intermediateFile.Name())
			return ""
		}
		h2 := sha1.New()
		_, sha1Error := io.Copy(h2, intermediateFile)
		if sha1Error != nil {
			fmt.Println("❌  Error calculate SHA1", sha1Error, r.URL.Path, intermediateFile.Name(), r.ContentLength)
			return ""
		}
		uploadUrl, uploadId, uploadFileId, flashUpload = UpdateFileFile(token, driveId, fileName, parentId, strconv.FormatInt(r.ContentLength, 10), int(count), strings.ToUpper(hex.EncodeToString(h2.Sum(nil))), proof, flashUpload)
		if flashUpload && (uploadFileId != "") {
			fmt.Println("⚡️⚡️  Rapid Upload ", r.URL.Path, r.ContentLength)
			//UploadFileComplete(token, driveId, uploadId, uploadFileId, parentId)
			if va, ok := cache.GoCache.Get(parentId); ok {
				l := va.(model.FileListModel)
				l.Items = append(l.Items, GetFileDetail(token, driveId, uploadFileId))
				cache.GoCache.SetDefault(parentId, l)
			}
			return uploadFileId
		}
	} else {
		uploadUrl, uploadId, uploadFileId, flashUpload = UpdateFileFile(token, driveId, fileName, parentId, strconv.FormatInt(r.ContentLength, 10), int(count), "", "", false)
	}

	if len(uploadUrl) == 0 {
		fmt.Println("❌ ❌  Empty UploadUrl", r.URL.Path, r.ContentLength, uploadId, uploadFileId)
		return ""
	}
	var bg time.Time = time.Now()
	stat, err := intermediateFile.Stat()
	if err != nil {
		fmt.Println("❌ can't stat file", err, r.URL.Path)
		return ""
	}

	fmt.Println("📢  Normal upload ", fileName, uploadId, r.ContentLength, stat.Size())
	_, e1 := intermediateFile.Seek(0, 0)
	if e1 != nil {
		fmt.Println("❌ ❌ Seek err", e1, r.URL.Path)
		return ""
	}
	for i := 0; i < int(count); i++ {
		fmt.Println("📢  Uploading part:", i+1, "Total:", count, r.URL.Path)
		pstart := time.Now()
		var dataByte []byte
		if int(count) == 1 {
			dataByte = make([]byte, r.ContentLength)
		} else if i == int(count)-1 {
			dataByte = make([]byte, r.ContentLength-int64(i)*DEFAULT)
		} else {
			dataByte = make([]byte, DEFAULT)
		}
		_, err := io.ReadFull(intermediateFile, dataByte)
		if err != nil {
			fmt.Println("❌  error reading from temp file", err, intermediateFile.Name(), r.URL.Path, uploadId)
			return ""
		}
		//check if upload url has expired
		uri := uploadUrl[i].Str
		idx := strings.Index(uri, "x-oss-expires=") + len("x-oss-expires=")
		idx2 := strings.Index(uri[idx:], "&")
		exp := uri[idx : idx2+idx]
		expire, _ := strconv.ParseInt(exp, 10, 64)
		if time.Now().UnixMilli()/1000 > expire {
			fmt.Println("⚠️     Now:", time.Now().UnixMilli()/1000)
			fmt.Println("⚠️  Expire:", exp)
			fmt.Println("⚠️  Uploading URL expired, renewing", uploadId, uploadFileId, r.URL.Path)
			for i := 0; i < 10; i++ {
				uploadUrl = GetUploadUrls(token, driveId, uploadFileId, uploadId, int(count))
				if len(uploadUrl) == int(count) {
					break
				}
				fmt.Println("Retry in 10 seconds")
				time.Sleep(10 * time.Second)
			}

			if len(uploadUrl) == 0 {
				fmt.Println("❌  Renew Uploading URL failed", r.URL.Path, uploadId, uploadFileId, "cancel upload")
				return ""
			} else {
				//fmt.Println("ℹ️  从头再来 💃🤔⬆️‼️ Resetting upload part")
				//i = 0
				fmt.Println("  💻  Renew Upload URL Done, Total Parts", len(uploadUrl))
			}
		}
		if ok := UploadFile(uploadUrl[i].Str, token, dataByte); !ok {
			fmt.Println("❌  Upload part failed ", r.URL.Path, "Part#", i+1, " 😜   Cancel upload")
			return ""
		}
		fmt.Println("✅  Done part:", i+1, "Elapsed:", time.Now().Sub(pstart).String(), r.URL.Path)

	}
	fmt.Println("⚡ ⚡ ⚡   Done. Elapsed ", time.Now().Sub(bg).String(), r.URL.Path, r.ContentLength)
	UploadFileComplete(token, driveId, uploadId, uploadFileId, parentId)
	if va, ok := cache.GoCache.Get(parentId); ok {
		l := va.(model.FileListModel)
		l.Items = append(l.Items, GetFileDetail(token, driveId, uploadFileId))
		cache.GoCache.SetDefault(parentId, l)
	}
	return uploadFileId
}
