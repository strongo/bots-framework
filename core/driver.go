package bots

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/DebtsTracker/translations/emoji"
	"github.com/julienschmidt/httprouter"
	"github.com/pkg/errors"
	"github.com/strongo/app"
	"github.com/strongo/log"
	"github.com/strongo/gamp"
)

// WebhookDriver is doing initial request & final response processing.
// That includes logging, creating input messages in a general format, sending response.
type WebhookDriver interface {
	RegisterWebhookHandlers(httpRouter *httprouter.Router, pathPrefix string, webhookHandlers ...WebhookHandler)
	HandleWebhook(w http.ResponseWriter, r *http.Request, webhookHandler WebhookHandler)
}

// BotDriver keeps information about bots and map requests to appropriate handlers
type BotDriver struct {
	Analytics  AnalyticsSettings
	botHost    BotHost
	appContext BotAppContext
	//router          *WebhooksRouter
	panicTextFooter string
}

var _ WebhookDriver = (*BotDriver)(nil) // Ensure BotDriver is implementing interface WebhookDriver

// AnalyticsSettings keeps data for Google Analytics
type AnalyticsSettings struct {
	GaTrackingID string // TODO: Refactor to list of analytics providers
	Enabled      func(r *http.Request) bool
}

// NewBotDriver registers new bot driver (TODO: describe why we need it)
func NewBotDriver(gaSettings AnalyticsSettings, appContext BotAppContext, host BotHost, panicTextFooter string) WebhookDriver {
	if appContext.AppUserEntityKind() == "" {
		panic("appContext.AppUserEntityKind() is empty")
	}
	if host == nil {
		panic("BotHost == nil")
	}
	return BotDriver{
		Analytics:  gaSettings,
		appContext: appContext,
		botHost:    host,
		//router: router,
		panicTextFooter: panicTextFooter,
	}
}

// RegisterWebhookHandlers adds handlers to a bot driver
func (d BotDriver) RegisterWebhookHandlers(httpRouter *httprouter.Router, pathPrefix string, webhookHandlers ...WebhookHandler) {
	for _, webhookHandler := range webhookHandlers {
		webhookHandler.RegisterWebhookHandler(d, d.botHost, httpRouter, pathPrefix)
	}
}

// HandleWebhook takes and HTTP request and process it
func (d BotDriver) HandleWebhook(w http.ResponseWriter, r *http.Request, webhookHandler WebhookHandler) {
	started := time.Now()
	c := d.botHost.Context(r)
	//log.Debugf(c, "BotDriver.HandleWebhook()")
	if w == nil {
		panic("Parameter 'w http.ResponseWriter' is nil")
	}
	if r == nil {
		panic("Parameter 'r *http.Request' is nil")
	}
	if webhookHandler == nil {
		panic("Parameter 'webhookHandler WebhookHandler' is nil")
	}

	botContext, entriesWithInputs, err := webhookHandler.GetBotContextAndInputs(c, r)

	if err != nil {
		if _, ok := err.(ErrAuthFailed); ok {
			log.Warningf(c, "Auth failed: %v", err)
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		} else if errors.Cause(err) == ErrNotImplemented {
			log.Debugf(c, err.Error())
			w.WriteHeader(http.StatusNoContent)
			//http.Error(w, "", http.StatusOK) // TODO: Decide how to handle it properly, return http.StatusNotImplemented?
		} else if _, ok := err.(*json.SyntaxError); ok {
			log.Debugf(c, errors.Wrap(err, "Request body is not valid JSON").Error())
			http.Error(w, err.Error(), http.StatusBadRequest)
		} else {
			log.Errorf(c, "Failed to call webhookHandler.GetBotContextAndInputs(router): %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	if botContext == nil {
		if entriesWithInputs == nil {
			log.Warningf(c, "botContext == nil, entriesWithInputs == nil")
		} else if len(entriesWithInputs) == 0 {
			log.Warningf(c, "botContext == nil, len(entriesWithInputs) == 0")
		} else {
			log.Errorf(c, "botContext == nil, len(entriesWithInputs) == %v", len(entriesWithInputs))
		}
		return
	}

	if entriesWithInputs == nil {
		log.Errorf(c, "entriesWithInputs == nil")
		return
	}

	log.Debugf(c, "BotDriver.HandleWebhook() => botCode=%v, len(entriesWithInputs): %d", botContext.BotSettings.Code, len(entriesWithInputs))

	switch botContext.BotSettings.Env {
	case strongo.EnvLocal:
		if r.Host != "localhost" && !strings.HasSuffix(r.Host, ".ngrok.io") {
			log.Warningf(c, "whc.GetBotSettings().Mode == Local, host: %v", r.Host)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
	case strongo.EnvProduction:
		if r.Host == "localhost" || strings.HasSuffix(r.Host, ".ngrok.io") {
			log.Warningf(c, "whc.GetBotSettings().Mode == Production, host: %v", r.Host)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
	}
	
	var (
		whc               WebhookContext // TODO: How do deal with Facebook multiple entries per request?
		measurementSender *gamp.BufferedClient
	)

	var sendStats bool
	{ // Initiate Google Analytics Measurement API client
		if d.Analytics.Enabled == nil {
			sendStats = botContext.BotSettings.Env == strongo.EnvProduction
			//log.Debugf(c, "d.AnalyticsSettings.Enabled == nil, botContext.BotSettings.Env: %v, sendStats: %v", strongo.EnvironmentNames[botContext.BotSettings.Env], sendStats)
		} else {
			sendStats = d.Analytics.Enabled(r)
			//log.Debugf(c, "d.AnalyticsSettings.Enabled != nil, sendStats: %v", sendStats)
		}
		if sendStats {
			botHost := botContext.BotHost
			measurementSender = gamp.NewBufferedClient("", botHost.GetHttpClient(c), nil)
		}
	}

	defer func() {
		log.Debugf(c, "driver.deferred(recover) - checking for panic & flush GA")
		if sendStats {
			measurementSender.Queue(gamp.NewTiming(time.Now().Sub(started)))
		}

		reportError := func(recovered interface{}) {
			messageText := fmt.Sprintf("Server error (panic): %v\n\n%v", recovered, d.panicTextFooter)
			log.Criticalf(c, "Panic recovered: %s\n%s", messageText, debug.Stack())

			if sendStats { // Zero if GA is disabled
				gaMessage := gamp.NewException(messageText, true)

				if whc != nil { // TODO: How do deal with Facebook multiple entries per request?
					gaMessage.Common = whc.GaCommon()
				} else {
					gaMessage.Common.ClientID = "c7ea15eb-3333-4d47-a002-9d1a14996371" // TODO: move hardcoded value
					gaMessage.Common.DataSource = "bot-" + whc.BotPlatform().Id()
				}

				if err := measurementSender.Queue(gaMessage); err != nil {
					log.Errorf(c, "Failed to queue exception details for GA: %v", err)
				} else {
					log.Debugf(c, "Exception details queued for GA.")
				}

				if err = measurementSender.Flush(); err != nil {
					log.Errorf(c, "Failed to send exception details to GA: %v", err)
				} else {
					log.Debugf(c, "Exception details sent to GA.")
				}
			}

			if whc != nil {
				if chatID, err := whc.BotChatID(); err == nil && chatID != "" {
					if responder := whc.Responder(); responder != nil {
						if _, err := responder.SendMessage(c, whc.NewMessage(emoji.ERROR_ICON+" "+messageText), BotApiSendMessageOverResponse); err != nil {
							log.Errorf(c, errors.WithMessage(err, "failed to report error to user").Error())
						}
					}
				}
			}
		}

		if recovered := recover(); recovered != nil {
			reportError(recovered)
		} else if sendStats {
			if err = measurementSender.Flush(); err != nil {
				log.Warningf(c, "Failed to flush to GA: %v", err)
			} else {
				log.Debugf(c, "Sent to GA: %v items", measurementSender.QueueDepth())
			}
		}
	}()

	if err != nil {
		log.Errorf(c, "Failed to create new WebhookContext: %v", err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	botCoreStores := webhookHandler.CreateBotCoreStores(d.appContext, r)
	defer func() {
		if whc != nil { // TODO: How do deal with Facebook multiple entries per request?
			//log.Debugf(c, "Closing BotChatStore...")
			//chatEntity := whc.ChatEntity()
			//if chatEntity != nil && chatEntity.GetPreferredLanguage() == "" {
			//	chatEntity.SetPreferredLanguage(whc.Locale().Code5)
			//}
			if err := botCoreStores.BotChatStore.Close(c); err != nil {
				log.Errorf(c, "Failed to close BotChatStore: %v", err)
				var m MessageFromBot
				m.Text = emoji.ERROR_ICON + " ERROR: Service is temporary unavailable. Probably a global outage, status at https://status.cloud.google.com/"
				if _, err := whc.Responder().SendMessage(c, m, BotApiSendMessageOverHTTPS); err != nil {
					log.Errorf(c, "Failed to report outage: %v", err)
				}
			}
		}
	}()

	logInput := func(i int, input WebhookInput) {
		switch input.(type) {
		case WebhookTextMessage:
			sender := input.GetSender()
			log.Debugf(c, "BotUser#%v(%v %v) => text: %v", sender.GetID(), sender.GetFirstName(), sender.GetLastName(), input.(WebhookTextMessage).Text())
		case WebhookNewChatMembersMessage:
			newMembers := input.(WebhookNewChatMembersMessage).NewChatMembers()
			var b bytes.Buffer
			b.WriteString(fmt.Sprintf("NewChatMembers: %d", len(newMembers)))
			for i, member := range newMembers {
				b.WriteString(fmt.Sprintf("\t%d: (%v) - %v %v", i+1, member.GetUserName(), member.GetFirstName(), member.GetLastName()))
			}
			log.Debugf(c, b.String())
		case WebhookContactMessage:
			sender := input.GetSender()
			contactMessage := input.(WebhookContactMessage)
			log.Debugf(c, "BotUser#%v(%v %v) => Contact(name: %v|%v, phone number: %v)", sender.GetID(), sender.GetFirstName(), sender.GetLastName(), contactMessage.FirstName(), contactMessage.LastName(), contactMessage.PhoneNumber())
		case WebhookCallbackQuery:
			callbackQuery := input.(WebhookCallbackQuery)
			callbackData := callbackQuery.GetData()
			sender := input.GetSender()
			log.Debugf(c, "BotUser#%v(%v %v) => callback: %v", sender.GetID(), sender.GetFirstName(), sender.GetLastName(), callbackData)
		case WebhookInlineQuery:
			sender := input.GetSender()
			log.Debugf(c, "BotUser#%v(%v %v) => inline query: %v", sender.GetID(), sender.GetFirstName(), sender.GetLastName(), input.(WebhookInlineQuery).GetQuery())
		case WebhookChosenInlineResult:
			sender := input.GetSender()
			log.Debugf(c, "BotUser#%v(%v %v) => choosen InlineMessageID: %v", sender.GetID(), sender.GetFirstName(), sender.GetLastName(), input.(WebhookChosenInlineResult).GetInlineMessageID())
		case WebhookReferralMessage:
			sender := input.GetSender()
			log.Debugf(c, "BotUser#%v(%v %v) => text: %v", sender.GetID(), sender.GetFirstName(), sender.GetLastName(), input.(WebhookTextMessage).Text())
		default:
			log.Warningf(c, "Unhandled input[%v] type: %T", i, input)
		}
	}

	dispatch := botContext.BotSettings.Router.Dispatch

	for _, entryWithInputs := range entriesWithInputs {
		for i, input := range entryWithInputs.Inputs {
			if input == nil {
				panic(fmt.Sprintf("entryWithInputs.Inputs[%d] == nil", i))
			}
			logInput(i, input)
			whc = webhookHandler.CreateWebhookContext(d.appContext, r, *botContext, input, botCoreStores, measurementSender)
			responder := webhookHandler.GetResponder(w, whc) // TODO: Move inside webhookHandler.CreateWebhookContext()?
			dispatch(responder, whc)
		}
	}
}
