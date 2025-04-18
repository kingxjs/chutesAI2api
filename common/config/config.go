package config

import (
	"chutesai2api/common/env"
	"os"
	"strings"
	"time"
)

// var BackendSecret = os.Getenv("BACKEND_SECRET")
var MysqlDsn = os.Getenv("MYSQL_DSN")
var IpBlackList = strings.Split(os.Getenv("IP_BLACK_LIST"), ",")
var ApiSecret = os.Getenv("API_SECRET")
var ApiSecrets = strings.Split(os.Getenv("API_SECRET"), ",")

// var DebugSQLEnabled = strings.ToLower(os.Getenv("DEBUG_SQL")) == "true"
var ProxyUrl = env.String("PROXY_URL", "")

// 过盾
var RecaptchaProxyUrl = env.String("RECAPTCHA_PROXY_URL", "")
var RecaptchaProxyClientKey = env.String("RECAPTCHA_PROXY_CLENT_KEY", "")

//var ChatMaxDays = env.Int("CHAT_MAX_DAYS", -1)

// 隐藏思考过程
var ReasoningHide = env.Int("REASONING_HIDE", 0)

// 前置message
var PRE_MESSAGES_JSON = env.String("PRE_MESSAGES_JSON", "")

// 路由前缀
var RoutePrefix = env.String("ROUTE_PREFIX", "")
var SwaggerEnable = os.Getenv("SWAGGER_ENABLE")

//var BackendApiEnable = env.Int("BACKEND_API_ENABLE", 1)

var DebugEnabled = os.Getenv("DEBUG") == "true"

var RateLimitKeyExpirationDuration = 20 * time.Minute

var RequestOutTimeDuration = 5 * time.Minute

var (
	RequestRateLimitNum            = env.Int("REQUEST_RATE_LIMIT", 60)
	RequestRateLimitDuration int64 = 1 * 60
)
