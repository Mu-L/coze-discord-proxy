package discord

import (
	"bytes"
	"context"
	"coze-discord-proxy/common"
	"coze-discord-proxy/common/myerr"
	"coze-discord-proxy/model"
	"coze-discord-proxy/telegram"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/bwmarrin/discordgo"
	"github.com/gin-gonic/gin"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/h2non/filetype"
	"golang.org/x/net/proxy"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var BotToken = os.Getenv("BOT_TOKEN")
var CozeBotId = os.Getenv("COZE_BOT_ID")
var GuildId = os.Getenv("GUILD_ID")
var ChannelId = os.Getenv("CHANNEL_ID")
var DefaultChannelEnable = os.Getenv("DEFAULT_CHANNEL_ENABLE")
var MessageMaxToken = os.Getenv("MESSAGE_MAX_TOKEN")
var ProxyUrl = os.Getenv("PROXY_URL")
var ChannelAutoDelTime = os.Getenv("CHANNEL_AUTO_DEL_TIME")
var CozeBotStayActiveEnable = os.Getenv("COZE_BOT_STAY_ACTIVE_ENABLE")
var UserAgent = os.Getenv("USER_AGENT")
var UserAuthorization = os.Getenv("USER_AUTHORIZATION")
var UserAuthorizations = strings.Split(UserAuthorization, ",")

var NoAvailableUserAuthChan = make(chan string)
var CreateChannelRiskChan = make(chan string)

var NoAvailableUserAuthPreNotifyTime time.Time
var CreateChannelRiskPreNotifyTime time.Time

var BotConfigList []*model.BotConfig
var BotConfigExist bool

var RepliesChans = make(map[string]chan model.ReplyResp)
var RepliesOpenAIChans = make(map[string]*model.OpenAIChatCompletionChan)
var RepliesOpenAIImageChans = make(map[string]chan model.OpenAIImagesGenerationResponse)

var ReplyStopChans = make(map[string]chan model.ChannelStopChan)
var Session *discordgo.Session

func StartBot(ctx context.Context, token string) {
	var err error
	Session, err = discordgo.New("Bot " + token)

	if err != nil {
		common.FatalLog("error creating Discord session,", err)
		return
	}

	if ProxyUrl != "" {
		proxyParse, client, err := NewProxyClient(ProxyUrl)
		if err != nil {
			common.FatalLog("error creating proxy client,", err)
		}
		Session.Client = client
		Session.Dialer.Proxy = http.ProxyURL(proxyParse)
		common.SysLog("Proxy Set Success!")
	}
	// 注册消息处理函数
	Session.AddHandler(messageCreate)
	Session.AddHandler(messageUpdate)

	// 打开websocket连接并开始监听
	err = Session.Open()
	if err != nil {
		common.FatalLog("error opening connection,", err)
		return
	}
	// 读取机器人配置文件
	loadBotConfig()
	// 验证docker配置文件
	checkEnvVariable()
	common.SysLog("Bot is now running. Enjoy It.")

	// 每日9点 重新加载BotConfig
	go loadBotConfigTask()
	// 每日9.5点 重新加载userAuth
	go loadUserAuthTask()

	if CozeBotStayActiveEnable == "1" || CozeBotStayActiveEnable == "" {
		// 开启coze保活任务 每日9.10
		go stayActiveMessageTask()
	}

	if telegram.NotifyTelegramBotToken != "" && telegram.TgBot != nil {
		// 开启tgbot消息推送任务
		go telegramNotifyMsgTask()
	}

	go func() {
		<-ctx.Done()
		if err := Session.Close(); err != nil {
			common.FatalLog("error closing Discord session,", err)
		}
	}()

	// 等待信号
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt, os.Kill)
	<-sc
}

func loadBotConfigTask() {
	for {
		source := rand.NewSource(time.Now().UnixNano())
		randomNumber := rand.New(source).Intn(60) // 生成0到60之间的随机整数

		// 计算距离下一个时间间隔
		now := time.Now()
		next := time.Date(now.Year(), now.Month(), now.Day(), 9, 5, 0, 0, now.Location())

		// 如果当前时间已经超过9点，那么等待到第二天的9点
		if now.After(next) {
			next = next.Add(24 * time.Hour)
		}

		delay := next.Sub(now)

		// 等待直到下一个间隔
		time.Sleep(delay + time.Duration(randomNumber)*time.Second)

		common.SysLog("CDP Scheduled loadBotConfig Task Job Start!")
		loadBotConfig()
		common.SysLog("CDP Scheduled loadBotConfig Task Job  End!")

	}
}

func telegramNotifyMsgTask() {

	for NoAvailableUserAuthChan != nil || CreateChannelRiskChan != nil {
		select {
		case msg, ok := <-NoAvailableUserAuthChan:
			if ok && msg == "stop" {
				tgMsgConfig := tgbotapi.NewMessage(telegram.NotifyTelegramUserIdInt64, fmt.Sprintf("⚠️【CDP-服务通知】\n服务已无可用USER_AUTHORIZATION,请及时更换!"))
				err := telegram.SendMessage(&tgMsgConfig)
				if err != nil {
					common.LogWarn(nil, fmt.Sprintf("Telegram 推送消息异常 error:%s", err.Error()))
				} else {
					//NoAvailableUserAuthChan = nil // 停止监听ch1
					NoAvailableUserAuthPreNotifyTime = time.Now()
				}
			} else if !ok {
				NoAvailableUserAuthChan = nil // 如果ch1已关闭，停止监听
			}
		case msg, ok := <-CreateChannelRiskChan:
			if ok && msg == "stop" {
				tgMsgConfig := tgbotapi.NewMessage(telegram.NotifyTelegramUserIdInt64, fmt.Sprintf("⚠️【CDP-服务通知】\n服务BOT_TOKEN关联的BOT已被风控,请及时ResetToken并更换!"))
				err := telegram.SendMessage(&tgMsgConfig)
				if err != nil {
					common.LogWarn(nil, fmt.Sprintf("Telegram 推送消息异常 error:%s", err.Error()))
				} else {
					//CreateChannelRiskChan = nil
					CreateChannelRiskPreNotifyTime = time.Now()
				}
			} else if !ok {
				CreateChannelRiskChan = nil
			}
		}
	}

}

func loadUserAuthTask() {
	for {
		source := rand.NewSource(time.Now().UnixNano())
		randomNumber := rand.New(source).Intn(60) // 生成0到60之间的随机整数

		// 计算距离下一个时间间隔
		now := time.Now()
		next := time.Date(now.Year(), now.Month(), now.Day(), 9, 5, 0, 0, now.Location())

		// 如果当前时间已经超过9点，那么等待到第二天的9点
		if now.After(next) {
			next = next.Add(24 * time.Hour)
		}

		delay := next.Sub(now)

		// 等待直到下一个间隔
		time.Sleep(delay + time.Duration(randomNumber)*time.Second)

		common.SysLog("CDP Scheduled loadUserAuth Task Job Start!")
		UserAuthorizations = strings.Split(UserAuthorization, ",")
		common.LogInfo(context.Background(), fmt.Sprintf("UserAuths: %+v", UserAuthorizations))
		common.SysLog("CDP Scheduled loadUserAuth Task Job  End!")

	}
}

func checkEnvVariable() {
	if UserAuthorization == "" {
		common.FatalLog("环境变量 USER_AUTHORIZATION 未设置")
	}
	if BotToken == "" {
		common.FatalLog("环境变量 BOT_TOKEN 未设置")
	}
	if GuildId == "" {
		common.FatalLog("环境变量 GUILD_ID 未设置")
	}
	if DefaultChannelEnable == "1" && ChannelId == "" {
		common.FatalLog("环境变量 CHANNEL_ID 未设置")
	} else if DefaultChannelEnable == "0" || DefaultChannelEnable == "" {
		ChannelId = ""
	}
	if CozeBotId == "" {
		common.FatalLog("环境变量 COZE_BOT_ID 未设置")
	} else if Session.State.User.ID == CozeBotId {
		common.FatalLog("环境变量 COZE_BOT_ID 不可为当前服务 BOT_TOKEN 关联的 BOT_ID")
	}

	if ProxyUrl != "" {
		_, _, err := NewProxyClient(ProxyUrl)
		if err != nil {
			common.FatalLog("环境变量 PROXY_URL 设置有误")
		}
	}
	if ChannelAutoDelTime != "" {
		_, err := strconv.Atoi(ChannelAutoDelTime)
		if err != nil {
			common.FatalLog("环境变量 CHANNEL_AUTO_DEL_TIME 设置有误")
		}
	}

	if MessageMaxToken == "" {
		MessageMaxToken = strconv.Itoa(128 * 1000)
	} else {
		_, err := strconv.Atoi(MessageMaxToken)
		if err != nil {
			common.FatalLog("环境变量 MESSAGE_MAX_TOKEN 设置有误")
		}
	}

	if telegram.NotifyTelegramBotToken != "" {
		err := telegram.InitTelegramBot()
		if err != nil {
			common.FatalLog(fmt.Sprintf("环境变量 NotifyTelegramBotToken 设置有误 error:%s", err.Error()))
		}

		if telegram.NotifyTelegramUserId == "" {
			common.FatalLog("环境变量 NOTIFY_TELEGRAM_USER_ID 未设置")
		} else {
			telegram.NotifyTelegramUserIdInt64, err = strconv.ParseInt(telegram.NotifyTelegramUserId, 10, 64)
			if err != nil {
				common.FatalLog(fmt.Sprintf("环境变量 NOTIFY_TELEGRAM_USER_ID 设置有误 error:%s", err.Error()))
			}
		}
	}

	common.SysLog("Environment variable check passed.")
}

func loadBotConfig() {
	// 检查文件是否存在
	_, err := os.Stat("config/bot_config.json")
	if err != nil {
		if !os.IsNotExist(err) {
			common.SysError("载入bot_config.json文件异常")
		}
		BotConfigExist = false
		return
	}

	// 读取文件
	file, err := os.ReadFile("config/bot_config.json")
	if err != nil {
		common.FatalLog("error reading bot config file,", err)
	}
	if len(file) == 0 {
		return
	}

	// 解析JSON到结构体切片  并载入内存
	err = json.Unmarshal(file, &BotConfigList)
	if err != nil {
		common.FatalLog("Error parsing JSON:", err)
	}

	for _, botConfig := range BotConfigList {
		// 校验默认频道
		if DefaultChannelEnable == "1" && botConfig.ChannelId == "" {
			common.FatalLog("默认频道开关开启时,必须为每个Coze-Bot配置ChannelId")
		}
		// 校验MaxToken
		if botConfig.MessageMaxToken != "" {
			_, err := strconv.Atoi(botConfig.MessageMaxToken)
			if err != nil {
				common.FatalLog(fmt.Sprintf("messageMaxToken 必须为数字!"))
			}
		} else {
			botConfig.MessageMaxToken = strconv.Itoa(128 * 1000)
		}
	}

	BotConfigExist = true

	// 将结构体切片转换为 JSON 字符串
	jsonData, err := json.MarshalIndent(BotConfigList, "", "  ")
	if err != nil {
		common.FatalLog(fmt.Sprintf("Error marshalling BotConfigs: %v", err))
	}

	// 打印 JSON 字符串
	common.SysLog(fmt.Sprintf("载入配置文件成功 BotConfigs:\n%s", string(jsonData)))
}

// messageCreate handles the create messages in Discord.
func messageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	// 提前检查参考消息是否为 nil
	if m.ReferencedMessage == nil {
		return
	}

	// 尝试获取 stopChan
	stopChan, exists := ReplyStopChans[m.ReferencedMessage.ID]
	if !exists {
		//channel, myerr := Session.Channel(m.ChannelID)
		// 不存在则直接删除频道
		//if myerr != nil || strings.HasPrefix(channel.Name, "cdp-chat-") {
		//SetChannelDeleteTimer(m.ChannelID, 5*time.Minute)
		return
		//}
	}

	// 如果作者为 nil 或消息来自 bot 本身,则发送停止信号
	if m.Author == nil || m.Author.ID == s.State.User.ID {
		//SetChannelDeleteTimer(m.ChannelID, 5*time.Minute)
		stopChan <- model.ChannelStopChan{
			Id: m.ChannelID,
		}
		return
	}

	replyChan, exists := RepliesChans[m.ReferencedMessage.ID]
	if exists {
		reply := processMessageCreate(m)
		replyChan <- reply
	} else {
		replyOpenAIChan, exists := RepliesOpenAIChans[m.ReferencedMessage.ID]
		if exists {
			reply := processMessageCreateForOpenAI(m)
			reply.Model = replyOpenAIChan.Model
			replyOpenAIChan.Response <- reply
		} else {
			replyOpenAIImageChan, exists := RepliesOpenAIImageChans[m.ReferencedMessage.ID]
			if exists {
				reply := processMessageCreateForOpenAIImage(m)
				replyOpenAIImageChan <- reply
			} else {
				return
			}
		}
	}
	// data: {"id":"chatcmpl-8lho2xvdDFyBdFkRwWAcMpWWAgymJ","object":"chat.completion.chunk","created":1706380498,"model":"gpt-4-turbo-0613","system_fingerprint":null,"choices":[{"index":0,"delta":{"content":"？"},"logprobs":null,"finish_reason":null}]}
	// data :{"id":"1200873365351698694","object":"chat.completion.chunk","created":1706380922,"model":"COZE","choices":[{"index":0,"message":{"role":"assistant","content":"你好！有什么我可以帮您的吗？如果有任"},"logprobs":null,"finish_reason":"","delta":{"content":"吗？如果有任"}}],"usage":{"prompt_tokens":13,"completion_tokens":19,"total_tokens":32},"system_fingerprint":null}

	// 如果消息包含组件或嵌入,则发送停止信号
	if len(m.Message.Components) > 0 {

		var suggestions []string

		actionRow, _ := m.Message.Components[0].(*discordgo.ActionsRow)
		for _, component := range actionRow.Components {
			button := component.(*discordgo.Button)
			suggestions = append(suggestions, button.Label)
		}

		replyOpenAIChan, exists := RepliesOpenAIChans[m.ReferencedMessage.ID]
		if exists {
			reply := processMessageCreateForOpenAI(m)
			stopStr := "stop"
			reply.Choices[0].FinishReason = &stopStr
			reply.Suggestions = suggestions
			reply.Model = replyOpenAIChan.Model
			replyOpenAIChan.Response <- reply
		}

		replyOpenAIImageChan, exists := RepliesOpenAIImageChans[m.ReferencedMessage.ID]
		if exists {
			reply := processMessageCreateForOpenAIImage(m)
			reply.Suggestions = suggestions
			replyOpenAIImageChan <- reply
		}

		stopChan <- model.ChannelStopChan{
			Id: m.ChannelID,
		}
	}

	return
}

// messageUpdate handles the updated messages in Discord.
func messageUpdate(s *discordgo.Session, m *discordgo.MessageUpdate) {
	// 提前检查参考消息是否为 nil
	if m.ReferencedMessage == nil {
		return
	}

	// 尝试获取 stopChan
	stopChan, exists := ReplyStopChans[m.ReferencedMessage.ID]
	if !exists {
		channel, err := Session.Channel(m.ChannelID)
		// 不存在则直接删除频道
		if err != nil || strings.HasPrefix(channel.Name, "cdp-chat-") {
			return
		}
	}

	// 如果作者为 nil 或消息来自 bot 本身,则发送停止信号
	if m.Author == nil || m.Author.ID == s.State.User.ID {
		stopChan <- model.ChannelStopChan{
			Id: m.ChannelID,
		}
		return
	}

	replyChan, exists := RepliesChans[m.ReferencedMessage.ID]
	if exists {
		reply := processMessageUpdate(m)
		replyChan <- reply
	} else {
		replyOpenAIChan, exists := RepliesOpenAIChans[m.ReferencedMessage.ID]
		if exists {
			reply := processMessageUpdateForOpenAI(m)
			reply.Model = replyOpenAIChan.Model
			replyOpenAIChan.Response <- reply
		} else {
			replyOpenAIImageChan, exists := RepliesOpenAIImageChans[m.ReferencedMessage.ID]
			if exists {
				reply := processMessageUpdateForOpenAIImage(m)
				replyOpenAIImageChan <- reply
			} else {
				return
			}
		}
	}
	// data: {"id":"chatcmpl-8lho2xvdDFyBdFkRwWAcMpWWAgymJ","object":"chat.completion.chunk","created":1706380498,"model":"gpt-4-turbo-0613","system_fingerprint":null,"choices":[{"index":0,"delta":{"content":"？"},"logprobs":null,"finish_reason":null}]}
	// data :{"id":"1200873365351698694","object":"chat.completion.chunk","created":1706380922,"model":"COZE","choices":[{"index":0,"message":{"role":"assistant","content":"你好！有什么我可以帮您的吗？如果有任"},"logprobs":null,"finish_reason":"","delta":{"content":"吗？如果有任"}}],"usage":{"prompt_tokens":13,"completion_tokens":19,"total_tokens":32},"system_fingerprint":null}

	// 如果消息包含组件或嵌入,则发送停止信号
	if len(m.Message.Components) > 0 {

		var suggestions []string

		actionRow, _ := m.Message.Components[0].(*discordgo.ActionsRow)
		for _, component := range actionRow.Components {
			button := component.(*discordgo.Button)
			suggestions = append(suggestions, button.Label)
		}

		replyOpenAIChan, exists := RepliesOpenAIChans[m.ReferencedMessage.ID]
		if exists {
			reply := processMessageUpdateForOpenAI(m)
			stopStr := "stop"
			reply.Choices[0].FinishReason = &stopStr
			reply.Suggestions = suggestions
			reply.Model = replyOpenAIChan.Model
			replyOpenAIChan.Response <- reply
		}

		replyOpenAIImageChan, exists := RepliesOpenAIImageChans[m.ReferencedMessage.ID]
		if exists {
			reply := processMessageUpdateForOpenAIImage(m)
			reply.Suggestions = suggestions
			replyOpenAIImageChan <- reply
		}

		stopChan <- model.ChannelStopChan{
			Id: m.ChannelID,
		}
	}

	return
}

func SendMessage(c *gin.Context, channelID, cozeBotId, message, maxToken string) (*discordgo.Message, string, error) {
	var ctx context.Context
	if c == nil {
		ctx = context.Background()
	} else {
		ctx = c.Request.Context()
	}

	if Session == nil {
		common.LogError(ctx, "discord session is nil")
		return nil, "", fmt.Errorf("discord session not initialized")
	}

	//var sentMsg *discordgo.Message

	content := fmt.Sprintf("%s \n <@%s>", message, cozeBotId)

	content = strings.Replace(content, `\u0026`, "&", -1)
	content = strings.Replace(content, `\u003c`, "<", -1)
	content = strings.Replace(content, `\u003e`, ">", -1)

	tokens := common.CountTokens(content)
	maxTokenInt, err := strconv.Atoi(maxToken)
	if err != nil {
		common.LogError(ctx, fmt.Sprintf("error sending message: %s", err))
		return &discordgo.Message{}, "", fmt.Errorf("error sending message")
	}

	if tokens > maxTokenInt {
		common.LogError(ctx, fmt.Sprintf("prompt已超过限制,请分段发送 [%v] %s", tokens, content))
		return nil, "", fmt.Errorf("prompt已超过限制,请分段发送 [%v]", tokens)
	}

	if len(UserAuthorizations) == 0 {
		//SetChannelDeleteTimer(channelID, 5*time.Second)
		common.LogError(ctx, fmt.Sprintf("无可用的 user_auth"))

		// tg发送通知
		if !common.IsSameDay(NoAvailableUserAuthPreNotifyTime, time.Now()) && telegram.NotifyTelegramBotToken != "" && telegram.TgBot != nil {
			go func() {
				NoAvailableUserAuthChan <- "stop"
			}()
		}

		return nil, "", fmt.Errorf("no_available_user_auth")
	}

	userAuth, err := common.RandomElement(UserAuthorizations)
	if err != nil {
		return nil, "", err
	}

	for i, sendContent := range common.ReverseSegment(content, 1990) {
		//sentMsg, myerr := Session.ChannelMessageSend(channelID, sendContent)
		//sentMsgId := sentMsg.ID
		// 4.0.0 版本下 用户端发送消息
		sendContent = strings.ReplaceAll(sendContent, "\\n", "\n")
		sentMsgId, err := SendMsgByAuthorization(c, userAuth, sendContent, channelID)
		if err != nil {
			var myErr *myerr.DiscordUnauthorizedError
			if errors.As(err, &myErr) {
				// 无效则将此 auth 移除
				UserAuthorizations = common.FilterSlice(UserAuthorizations, userAuth)
				return SendMessage(c, channelID, cozeBotId, message, maxToken)
			}
			common.LogError(ctx, fmt.Sprintf("error sending message: %s", err))
			return nil, "", fmt.Errorf("error sending message")
		}

		time.Sleep(1 * time.Second)

		if i == len(common.ReverseSegment(content, 1990))-1 {
			return &discordgo.Message{
				ID: sentMsgId,
			}, userAuth, nil
		}
	}
	return &discordgo.Message{}, "", fmt.Errorf("error sending message")
}

func ThreadStart(channelId, threadName string, archiveDuration int) (string, error) {
	// 创建新的线程
	th, err := Session.ThreadStart(channelId, threadName, discordgo.ChannelTypeGuildText, archiveDuration)

	if err != nil {
		common.LogError(context.Background(), fmt.Sprintf("创建线程时异常 %s", err.Error()))
		return "", err
	}
	return th.ID, nil
}

func NewProxyClient(proxyUrl string) (proxyParse *url.URL, client *http.Client, err error) {

	proxyParse, err = url.Parse(proxyUrl)
	if err != nil {
		common.FatalLog("代理地址设置有误")
	}

	if strings.HasPrefix(proxyParse.Scheme, "http") {
		httpTransport := &http.Transport{
			Proxy: http.ProxyURL(proxyParse),
		}
		return proxyParse, &http.Client{
			Transport: httpTransport,
		}, nil
	} else if strings.HasPrefix(proxyParse.Scheme, "sock") {
		dialer, err := proxy.SOCKS5("tcp", proxyParse.Host, nil, proxy.Direct)
		if err != nil {
			log.Fatal("Error creating dialer, ", err)
		}

		dialContext := func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.Dial(network, addr)
		}

		// 使用该拨号器创建一个 HTTP 客户端
		httpClient := &http.Client{
			Transport: &http.Transport{
				DialContext: dialContext,
			},
		}

		return proxyParse, httpClient, nil
	} else {
		return nil, nil, fmt.Errorf("仅支持sock和http代理！")
	}

}

func stayActiveMessageTask() {
	for {
		source := rand.NewSource(time.Now().UnixNano())
		randomNumber := rand.New(source).Intn(60) // 生成0到60之间的随机整数

		// 计算距离下一个时间间隔
		now := time.Now()
		// 9点10分 为了保证loadUserAuthTask每日任务执行完毕
		next := time.Date(now.Year(), now.Month(), now.Day(), 9, 10, 0, 0, now.Location())

		// 如果当前时间已经超过9点，那么等待到第二天的9点
		if now.After(next) {
			next = next.Add(24 * time.Hour)
		}

		delay := next.Sub(now)

		// 等待直到下一个间隔
		time.Sleep(delay + time.Duration(randomNumber)*time.Second)

		var taskBotConfigs = BotConfigList

		taskBotConfigs = append(taskBotConfigs, &model.BotConfig{
			ChannelId:       ChannelId,
			CozeBotId:       CozeBotId,
			MessageMaxToken: MessageMaxToken,
		})

		taskBotConfigs = model.FilterUniqueBotChannel(taskBotConfigs)

		common.SysLog("CDP Scheduled Task Job Start!")
		var sendChannelList []string
		for _, config := range taskBotConfigs {
			var sendChannelId string
			var err error
			if config.ChannelId == "" {
				nextID, _ := common.NextID()
				sendChannelId, err = CreateChannelWithRetry(nil, GuildId, fmt.Sprintf("cdp-chat-%s", nextID), 0)
				if err != nil {
					common.LogError(nil, err.Error())
					break
				}
				sendChannelList = append(sendChannelList, sendChannelId)
			} else {
				sendChannelId = config.ChannelId
			}
			nextID, err := common.NextID()
			if err != nil {
				common.SysError(fmt.Sprintf("ChannelId{%s} BotId{%s} 活跃机器人任务消息发送异常!雪花Id生成失败!", sendChannelId, config.CozeBotId))
				continue
			}
			_, _, err = SendMessage(nil, sendChannelId, config.CozeBotId, fmt.Sprintf("【%v】 %s", nextID, "CDP Scheduled Task Job Send Msg Success!"), config.MessageMaxToken)
			if err != nil {
				common.SysError(fmt.Sprintf("ChannelId{%s} BotId{%s} 活跃机器人任务消息发送异常!", sendChannelId, config.CozeBotId))
			} else {
				common.SysLog(fmt.Sprintf("ChannelId{%s} BotId{%s} 活跃机器人任务消息发送成功!", sendChannelId, config.CozeBotId))
			}
			time.Sleep(5 * time.Second)
		}
		for _, channelId := range sendChannelList {
			ChannelDel(channelId)
		}
		common.SysLog("CDP Scheduled Task Job End!")

	}
}

func UploadToDiscordAndGetURL(channelID string, base64Data string) (string, error) {

	// 获取";base64,"后的Base64编码部分
	dataParts := strings.Split(base64Data, ";base64,")
	if len(dataParts) != 2 {
		return "", fmt.Errorf("")
	}
	base64Data = dataParts[1]

	data, err := base64.StdEncoding.DecodeString(base64Data)
	if err != nil {
		return "", err
	}
	// 创建一个新的文件读取器
	file := bytes.NewReader(data)

	kind, err := filetype.Match(data)

	if err != nil {
		return "", fmt.Errorf("无法识别的文件类型")
	}

	// 创建一个新的 MessageSend 结构
	m := &discordgo.MessageSend{
		Files: []*discordgo.File{
			{
				Name:   fmt.Sprintf("file-%s.%s", common.GetTimeString(), kind.Extension),
				Reader: file,
			},
		},
	}

	// 发送消息
	message, err := Session.ChannelMessageSendComplex(channelID, m)
	if err != nil {
		return "", err
	}

	// 检查消息中是否包含附件,并获取 URL
	if len(message.Attachments) > 0 {
		return message.Attachments[0].URL, nil
	}

	return "", fmt.Errorf("no attachment found in the message")
}

// FilterConfigs 根据proxySecret和channelId过滤BotConfig
func FilterConfigs(configs []*model.BotConfig, secret, gptModel string, channelId *string) []*model.BotConfig {
	var filteredConfigs []*model.BotConfig
	for _, config := range configs {
		matchSecret := secret == "" || config.ProxySecret == secret
		matchGptModel := gptModel == "" || common.SliceContains(config.Model, gptModel)
		matchChannelId := channelId == nil || *channelId == "" || config.ChannelId == *channelId
		if matchSecret && matchChannelId && matchGptModel {
			filteredConfigs = append(filteredConfigs, config)
		}
	}
	return filteredConfigs
}

func DelLimitBot(botId string) {
	if BotConfigExist {
		BotConfigList = FilterBotConfigByBotId(BotConfigList, botId)
	} else {
		if CozeBotId == botId {
			CozeBotId = ""
		}
	}
}

func FilterBotConfigByBotId(slice []*model.BotConfig, filter string) []*model.BotConfig {
	var result []*model.BotConfig
	for _, value := range slice {
		if value.CozeBotId != filter {
			result = append(result, value)
		}
	}
	return result
}
