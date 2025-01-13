package tool

import (
	"crypto/sha256"
	"encoding/base64"
	"math/rand"
	"strconv"
	"time"
)

func GenToken(username string) string {
	data := username + strconv.Itoa(int(time.Now().Unix())) + strconv.Itoa(rand.Int())
	hash := sha256.New()
	hash.Write([]byte(data))
	hash.Sum(nil)
	return base64.URLEncoding.EncodeToString(hash.Sum(nil))
}
