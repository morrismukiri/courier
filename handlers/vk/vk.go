package vk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/buger/jsonparser"
	"github.com/go-errors/errors"
	"github.com/nyaruka/courier"
	"github.com/nyaruka/courier/handlers"
	"github.com/nyaruka/courier/utils"
	"github.com/nyaruka/gocommon/urns"
	"github.com/sirupsen/logrus"
	"io"
	"io/ioutil"
	"net/http"
	"strconv"
	"time"
)

const (
	scheme = "vk"

	// callback API events
	eventTypeServerVerification = "confirmation"
	eventTypeNewMessage         = "message_new"

	configServerVerificationString = "callback_verification_string"

	// base API values
	apiBaseURL       = "https://api.vk.com/method"
	apiVersion       = "5.103"
	paramApiVersion  = "v"
	paramAccessToken = "access_token"

	// response check values
	responseIncomingMessage    = "ok"
	responseOutgoingMessageKey = "response"

	// send message
	URLSendMessage   = apiBaseURL + "/messages.send.json"
	paramUserId      = "user_id"
	paramMessage     = "message"
	paramAttachments = "attachment"
	paramRandomId    = "random_id"
)

func init() {
	courier.RegisterHandler(newHandler())
}

type handler struct {
	handlers.BaseHandler
}

func newHandler() courier.ChannelHandler {
	return &handler{handlers.NewBaseHandler(courier.ChannelType("VK"), "VK")}
}

func (h *handler) Initialize(s courier.Server) error {
	h.SetServer(s)
	s.AddHandlerRoute(h, http.MethodPost, "receive", h.receiveEvent)
	return nil
}

// base body of callback API event
type moPayload struct {
	Type      string `json:"type"   validate:"required"`
	SecretKey string `json:"secret" validate:"required"`
}

// body to new message event
type moNewMessagePayload struct {
	Object struct {
		Message struct {
			Id          int64           `json:"id" validate:"required"`
			Date        int64           `json:"date" validate:"required"`
			UserId      int64           `json:"from_id" validate:"required"`
			Text        string          `json:"text"`
			Attachments json.RawMessage `json:"attachments"`
			Geo         struct {
				Coords struct {
					Lat float64 `json:"latitude"`
					Lng float64 `json:"longitude"`
				} `json:"coordinates"`
			} `json:"geo"`
		} `json:"message" validate:"required"`
	} `json:"object" validate:"required"`
}

// receiveEvent handles request event type
func (h *handler) receiveEvent(ctx context.Context, channel courier.Channel, w http.ResponseWriter, r *http.Request) ([]courier.Event, error) {
	// read request body
	bodyBytes, err := ioutil.ReadAll(io.LimitReader(r.Body, 100000))

	if err != nil {
		return nil, handlers.WriteAndLogRequestError(ctx, h, channel, w, r, fmt.Errorf("unable to read request body: %s", err))
	}
	// restore body to its original state
	r.Body = ioutil.NopCloser(bytes.NewBuffer(bodyBytes))
	payload := &moPayload{}

	if err := json.Unmarshal(bodyBytes, payload); err != nil {
		return nil, handlers.WriteAndLogRequestError(ctx, h, channel, w, r, err)
	}
	// check shared secret key before proceeding
	secret := channel.StringConfigForKey(courier.ConfigSecret, "")

	if payload.SecretKey != secret {
		return nil, handlers.WriteAndLogRequestError(ctx, h, channel, w, r, errors.New("wrong secret key"))
	}
	// check event type and decode body to correspondent struct
	switch payload.Type {
	case eventTypeServerVerification:
		return h.verifyServer(channel, w)

	case eventTypeNewMessage:
		newMessage := &moNewMessagePayload{}

		if err := handlers.DecodeAndValidateJSON(newMessage, r); err != nil {
			return nil, handlers.WriteAndLogRequestError(ctx, h, channel, w, r, err)
		}
		return h.receiveMessage(ctx, channel, w, r, newMessage)

	default:
		return nil, handlers.WriteAndLogRequestIgnored(ctx, h, channel, w, r, "ignoring request, no message or server verification event")
	}
}

// verifyServer handles VK's callback verification
func (h *handler) verifyServer(channel courier.Channel, w http.ResponseWriter) ([]courier.Event, error) {
	verificationString := channel.StringConfigForKey(configServerVerificationString, "")
	// write required response
	_, err := fmt.Fprint(w, verificationString)

	return nil, err
}

// receiveMessage handles new message event
func (h *handler) receiveMessage(ctx context.Context, channel courier.Channel, w http.ResponseWriter, r *http.Request, payload *moNewMessagePayload) ([]courier.Event, error) {
	// get userPayload to set contact name
	userId := payload.Object.Message.UserId
	user, err := retrieveUser(channel, userId)
	contactName := ""

	if err != nil {
		logrus.WithField("channel_uuid", channel.UUID()).WithField("userPayload id", userId).Error("error getting VK userPayload", err)
	} else {
		contactName = fmt.Sprintf("%s %s", user.FirstName, user.LastName)
	}
	urn := urns.URN(fmt.Sprintf("%s:%d", scheme, payload.Object.Message.UserId))
	date := time.Unix(payload.Object.Message.Date, 0).UTC()
	text := payload.Object.Message.Text
	externalId := strconv.FormatInt(payload.Object.Message.Id, 10)
	msg := h.Backend().NewIncomingMsg(channel, urn, text).WithReceivedOn(date).WithContactName(contactName).WithExternalID(externalId)

	if attachment := takeFirstAttachmentUrl(*payload); attachment != "" {
		msg = msg.WithAttachment(attachment)
	}
	// save message to our backend
	if err := h.Backend().WriteMsg(ctx, msg); err != nil {
		return nil, handlers.WriteAndLogRequestError(ctx, h, channel, w, r, err)
	}
	// write required response
	_, err = fmt.Fprint(w, responseIncomingMessage)

	return []courier.Event{msg}, err
}

func (h *handler) SendMsg(ctx context.Context, msg courier.Msg) (courier.MsgStatus, error) {
	status := h.Backend().NewMsgStatusForID(msg.Channel(), msg.ID(), courier.MsgErrored)
	req, err := http.NewRequest(http.MethodPost, URLSendMessage, nil)

	if err != nil {
		return status, errors.New("Cannot create send message request")
	}
	params := buildApiBaseParams(msg.Channel())
	params.Set(paramUserId, msg.URN().Path())
	params.Set(paramMessage, msg.Text())
	params.Set(paramRandomId, msg.ID().String())

	// TODO
	attachmentsParam, _ := buildMsgAttachmentsParam(msg)
	params.Set(paramAttachments, attachmentsParam)

	req.URL.RawQuery = params.Encode()
	res, err := utils.MakeHTTPRequest(req)

	log := courier.NewChannelLogFromRR("Message Sent", msg.Channel(), msg.ID(), res).WithError("Message Send Error", err)
	status.AddLog(log)

	if err != nil {
		return status, err
	}
	externalMsgId, err := jsonparser.GetInt(res.Body, responseOutgoingMessageKey)

	if err != nil {
		return status, errors.Errorf("no '%s' value in response", responseOutgoingMessageKey)
	}
	status.SetExternalID(strconv.FormatInt(externalMsgId, 10))
	status.SetStatus(courier.MsgSent)

	return status, nil
}
