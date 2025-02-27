// Package sns provides helper functions for verifying and processing Amazon AWS SNS HTTP POST payloads.
package sns

import (
	"bytes"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"encoding/xml"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"time"
)

// https://docs.aws.amazon.com/ses/latest/dg/notification-contents.html
type JsonDateTime time.Time

func (j *JsonDateTime) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), "\"")
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return err
	}
	*j = JsonDateTime(t)
	return nil
}

func (j JsonDateTime) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Time(j))
}

type AmazonSesMail struct {
	Timestamp        JsonDateTime           `json:"timestamp"`
	Source           string                 `json:"source"`
	SourceArn        string                 `json:"sourceArn"`
	SourceIp         string                 `json:"sourceIp"`
	CallerIdentity   string                 `json:"callerIdentity"`
	SendingAccountId string                 `json:"sendingAccountId"`
	MessageId        string                 `json:"messageId"`
	Destination      []string               `json:"destination"`
	HeadersTruncated bool                   `json:"headersTruncated"`
	Headers          []AmazonSesMailHeaders `json:"headers"`
	CommonHeaders    map[string]interface{} `json:"commonHeaders"`
}

type AmazonSesMailHeaders struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type AmazonSesComplaintNotification struct {
	NotificationType string             `json:"notificationType"`
	Complaint        AmazonSesComplaint `json:"complaint"`
	Mail             AmazonSesMail      `json:"mail"`
}

type AmazonSesComplaint struct {
	ComplainedRecipients  []AmazonSesComplainedRecipient `json:"complainedRecipients"`
	Timestamp             JsonDateTime                   `json:"timestamp"`
	FeedbackId            string                         `json:"feedbackId"`
	ComplaintSubType      *string                        `json:"complaintSubType"`
	UserAgent             *string                        `json:"userAgent"`
	ComplaintFeedbackType *string                        `json:"complaintFeedbackType"`
	ArrivalDate           *JsonDateTime                  `json:"arrivalDate"`
}

type AmazonSesComplainedRecipient struct {
	EmailAddress string `json:"emailAddress"`
}

type AmazonSesBounceNotification struct {
	NotificationType string          `json:"notificationType"`
	Bounce           AmazonSesBounce `json:"bounce"`
	Mail             AmazonSesMail   `json:"mail"`
}

type AmazonSesBounce struct {
	BounceType        string                      `json:"bounceType"`
	BounceSubType     string                      `json:"bounceSubType"`
	BouncedRecipients []AmazonSesBouncedRecipient `json:"bouncedRecipients"`
	Timestamp         JsonDateTime                `json:"timestamp"`
	FeedbackId        string                      `json:"feedbackId"`
	RemoteMtaIp       *string                     `json:"remoteMtaIp"`
	ReportingMTA      *string                     `json:"reportingMTA"`
}

type AmazonSesBouncedRecipient struct {
	EmailAddress   string  `json:"emailAddress"`
	Action         *string `json:"action"`
	Status         *string `json:"status"`
	DiagnosticCode *string `json:"diagnosticCode"`
}

type AmazonSesDeliveryNotification struct {
	NotificationType string            `json:"notificationType"`
	Delivery         AmazonSesDelivery `json:"delivery"`
	Mail             AmazonSesMail     `json:"mail"`
}

type AmazonSesDelivery struct {
	Timestamp            JsonDateTime `json:"timestamp"`
	ProcessingTimeMillis int32        `json:"processingTimeMillis"`
	Recipients           []string     `json:"recipients"`
	SmtpResponse         string       `json:"smtpResponse"`
	ReportingMTA         string       `json:"reportingMTA"`
	RemoteMtaIp          string       `json:"remoteMtaIp"`
}

// https://github.com/robbiet480/go.sns/issues/2
var hostPattern = regexp.MustCompile(`^sns\.[a-zA-Z0-9\-]{3,}\.amazonaws\.com(\.cn)?$`)

// Payload contains a single POST from SNS
type Payload struct {
	Message          string `json:"Message"`
	MessageId        string `json:"MessageId"`
	Signature        string `json:"Signature"`
	SignatureVersion string `json:"SignatureVersion"`
	SigningCertURL   string `json:"SigningCertURL"`
	SubscribeURL     string `json:"SubscribeURL"`
	Subject          string `json:"Subject"`
	Timestamp        string `json:"Timestamp"`
	Token            string `json:"Token"`
	TopicArn         string `json:"TopicArn"`
	Type             string `json:"Type"`
	UnsubscribeURL   string `json:"UnsubscribeURL"`
}

// ConfirmSubscriptionResponse contains the XML response of accessing a SubscribeURL
type ConfirmSubscriptionResponse struct {
	XMLName         xml.Name `xml:"ConfirmSubscriptionResponse"`
	SubscriptionArn string   `xml:"ConfirmSubscriptionResult>SubscriptionArn"`
	RequestId       string   `xml:"ResponseMetadata>RequestId"`
}

// UnsubscribeResponse contains the XML response of accessing an UnsubscribeURL
type UnsubscribeResponse struct {
	XMLName   xml.Name `xml:"UnsubscribeResponse"`
	RequestId string   `xml:"ResponseMetadata>RequestId"`
}

// BuildSignature returns a byte array containing a signature usable for SNS verification
func (payload *Payload) BuildSignature() []byte {
	var builtSignature bytes.Buffer
	signableKeys := []string{"Message", "MessageId", "Subject", "SubscribeURL", "Timestamp", "Token", "TopicArn", "Type"}
	for _, key := range signableKeys {
		reflectedStruct := reflect.ValueOf(payload)
		field := reflect.Indirect(reflectedStruct).FieldByName(key)
		value := field.String()
		if field.IsValid() && value != "" {
			builtSignature.WriteString(key + "\n")
			builtSignature.WriteString(value + "\n")
		}
	}
	return builtSignature.Bytes()
}

// SignatureAlgorithm returns properly Algorithm for AWS Signature Version.
func (payload *Payload) SignatureAlgorithm() x509.SignatureAlgorithm {
	if payload.SignatureVersion == "2" {
		return x509.SHA256WithRSA
	}
	return x509.SHA1WithRSA
}

// VerifyPayload will verify that a payload came from SNS
func (payload *Payload) VerifyPayload() error {
	payloadSignature, err := base64.StdEncoding.DecodeString(payload.Signature)
	if err != nil {
		return err
	}

	certURL, err := url.Parse(payload.SigningCertURL)
	if err != nil {
		return err
	}

	if certURL.Scheme != "https" {
		return fmt.Errorf("url should be using https")
	}

	if !hostPattern.Match([]byte(certURL.Host)) {
		return fmt.Errorf("certificate is located on an invalid domain")
	}

	resp, err := http.Get(payload.SigningCertURL)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	decodedPem, _ := pem.Decode(body)
	if decodedPem == nil {
		return errors.New("The decoded PEM file was empty!")
	}

	parsedCertificate, err := x509.ParseCertificate(decodedPem.Bytes)
	if err != nil {
		return err
	}

	return parsedCertificate.CheckSignature(payload.SignatureAlgorithm(), payload.BuildSignature(), payloadSignature)
}

// Subscribe will use the SubscribeURL in a payload to confirm a subscription and return a ConfirmSubscriptionResponse
func (payload *Payload) Subscribe() (ConfirmSubscriptionResponse, error) {
	var response ConfirmSubscriptionResponse
	if payload.SubscribeURL == "" {
		return response, errors.New("Payload does not have a SubscribeURL!")
	}

	resp, err := http.Get(payload.SubscribeURL)
	if err != nil {
		return response, err
	}

	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return response, err
	}

	xmlErr := xml.Unmarshal(body, &response)
	if xmlErr != nil {
		return response, xmlErr
	}
	return response, nil
}

// Unsubscribe will use the UnsubscribeURL in a payload to confirm a subscription and return a UnsubscribeResponse
func (payload *Payload) Unsubscribe() (UnsubscribeResponse, error) {
	var response UnsubscribeResponse
	resp, err := http.Get(payload.UnsubscribeURL)
	if err != nil {
		return response, err
	}

	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return response, err
	}

	xmlErr := xml.Unmarshal(body, &response)
	if xmlErr != nil {
		return response, xmlErr
	}
	return response, nil
}
