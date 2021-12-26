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
		//dataTemp, _ := io.ReadAll(r.Body)
		//r.ContentLength = int64(len(dataTemp))
		return ""
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
		fmt.Println("❌  Error creating intermediate file ", fileName, intermediateFile.Name(), r.ContentLength)
		return ""
	}
	//大于150K小于25G的才开启闪传
	//由于webdav协议的局限性，使用中间文件，服务求要有足够的存储，否则会将硬盘撑爆掉
	if r.ContentLength > 1024*150 && r.ContentLength <= 1024*1024*1024*25 {
		preHashDataBytes := make([]byte, 1024)
		_, err := intermediateFile.ReadAt(preHashDataBytes, 0)
		if err != nil {
			fmt.Println("error reading file", intermediateFile.Name(), err)
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
			_, offerr := intermediateFile.ReadAt(off, offset)
			if offerr != nil {
				fmt.Println("Can't calculate proof", offerr)
				return ""
			}
			proof = utils.GetProof(off)
			flashUpload = true
		}
		_, seekError := intermediateFile.Seek(0, 0)
		if seekError != nil {
			fmt.Println("回不去了...", seekError, fileName, intermediateFile.Name())
			return ""
		}
		h2 := sha1.New()
		_, sha1Error := io.Copy(h2, intermediateFile)
		if sha1Error != nil {
			fmt.Println("Error calculate SHA1", sha1Error, fileName, intermediateFile.Name(), r.ContentLength)
			return ""
		}
		uploadUrl, uploadId, uploadFileId, flashUpload = UpdateFileFile(token, driveId, fileName, parentId, strconv.FormatInt(r.ContentLength, 10), int(count), strings.ToUpper(hex.EncodeToString(h2.Sum(nil))), proof, flashUpload)
		if flashUpload && (uploadFileId != "") {
			fmt.Println("⚡️⚡️  Rapid Upload ", fileName, r.ContentLength)
			//UploadFileComplete(token, driveId, uploadId, uploadFileId, parentId)
			cache.GoCache.Delete(parentId)
			return uploadFileId
		}
		//intermediateFile.Write(readBytes)
		//readBytes = nil
	} else {
		uploadUrl, uploadId, uploadFileId, flashUpload = UpdateFileFile(token, driveId, fileName, parentId, strconv.FormatInt(r.ContentLength, 10), int(count), "", "", false)
	}

	if len(uploadUrl) == 0 {
		return ""
	}
	var bg time.Time = time.Now()
	stat, err := intermediateFile.Stat()
	if err != nil {
		fmt.Println(err)
		return ""
	}

	fmt.Println("📢  Normal upload ", fileName, uploadId, r.ContentLength, stat.Size())
	intermediateFile.Seek(0, 0)
	for i := 0; i < int(count); i++ {
		fmt.Println("📢  Uploading part:", i+1, "total:", count, fileName, "total size:", r.ContentLength)
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
			fmt.Println("❌  err reading from temp file", err, intermediateFile.Name(), fileName, uploadId)
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
			fmt.Println("⚠️  Uploading URL expired, renewing", uploadId, uploadFileId, fileName)
			uploadUrl = GetUploadUrls(token, driveId, uploadFileId, uploadId, int(count))
			if len(uploadUrl) == 0 {
				fmt.Println("❌  Renew Uploading URL failed", fileName, uploadId, uploadFileId, "cancel upload")
				return ""
			}
		}
		if ok := UploadFile(uploadUrl[i].Str, token, dataByte); !ok {
			fmt.Println("❌  Upload part failed", fileName, "part", i+1, "cancel upload")
			return ""
		}
		fmt.Println("📢  Done part:", i+1, "total:", count+1, fileName, "total size:", r.ContentLength, "time elapsed:", time.Now().Sub(pstart).String())

	}
	fmt.Println("✅  Done, elapsed ", time.Now().Sub(bg).String(), fileName, r.ContentLength)
	UploadFileComplete(token, driveId, uploadId, uploadFileId, parentId)
	cache.GoCache.Delete(parentId)
	return uploadFileId
}
