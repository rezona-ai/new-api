package common

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/google/uuid"
)

type verificationValue struct {
	code string
	time time.Time
}

const (
	EmailVerificationPurpose = "v"
	PasswordResetPurpose     = "r"
)

var verificationMutex sync.Mutex
var verificationMap map[string]verificationValue
var verificationMapMaxSize = 10
var VerificationValidMinutes = 10

// redisVerificationKey 把 purpose+key 映射到带前缀的 Redis key，避免与其他缓存撞键。
func redisVerificationKey(key string, purpose string) string {
	return "verification:" + purpose + key
}

func GenerateVerificationCode(length int) string {
	code := uuid.New().String()
	code = strings.Replace(code, "-", "", -1)
	if length == 0 {
		return code
	}
	return code[:length]
}

func RegisterVerificationCodeWithKey(key string, code string, purpose string) {
	// 启用 Redis 时存 Redis，保证多实例 / 多副本部署下发码与校验共享存储。
	if RedisEnabled {
		// 不走 common.RedisSet：它在 DebugEnabled 时会打印 value=%s，会把验证码 / 找回密码 token 明文写进日志。
		// 这里直连 RDB.Set，并只在 Debug 时打印 key（不含 code），避免泄露。
		redisKey := redisVerificationKey(key, purpose)
		if DebugEnabled {
			SysLog("Redis SET verification key: " + redisKey)
		}
		err := RDB.Set(context.Background(), redisKey, code, time.Duration(VerificationValidMinutes)*time.Minute).Err()
		if err != nil {
			SysError("failed to store verification code in Redis: " + err.Error())
		}
		return
	}
	verificationMutex.Lock()
	defer verificationMutex.Unlock()
	verificationMap[purpose+key] = verificationValue{
		code: code,
		time: time.Now(),
	}
	if len(verificationMap) > verificationMapMaxSize {
		removeExpiredPairs()
	}
}

func VerifyCodeWithKey(key string, code string, purpose string) bool {
	if RedisEnabled {
		stored, err := RedisGet(redisVerificationKey(key, purpose))
		if err != nil {
			// redis.Nil 表示不存在或已过期；其他错误记日志，统一视为校验失败。
			if !errors.Is(err, redis.Nil) {
				SysError("failed to read verification code from Redis: " + err.Error())
			}
			return false
		}
		return code == stored
	}
	verificationMutex.Lock()
	defer verificationMutex.Unlock()
	value, okay := verificationMap[purpose+key]
	now := time.Now()
	if !okay || int(now.Sub(value.time).Seconds()) >= VerificationValidMinutes*60 {
		return false
	}
	return code == value.code
}

func DeleteKey(key string, purpose string) {
	if RedisEnabled {
		if err := RedisDel(redisVerificationKey(key, purpose)); err != nil {
			SysError("failed to delete verification code from Redis: " + err.Error())
		}
		return
	}
	verificationMutex.Lock()
	defer verificationMutex.Unlock()
	delete(verificationMap, purpose+key)
}

// no lock inside, so the caller must lock the verificationMap before calling!
func removeExpiredPairs() {
	now := time.Now()
	for key := range verificationMap {
		if int(now.Sub(verificationMap[key].time).Seconds()) >= VerificationValidMinutes*60 {
			delete(verificationMap, key)
		}
	}
}

func init() {
	verificationMutex.Lock()
	defer verificationMutex.Unlock()
	verificationMap = make(map[string]verificationValue)
}
