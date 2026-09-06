// Command kyber-mcp-slack bridges Slack Socket Mode events to Kyber's inbound
// binding and exposes a loopback-only MCP reply tool.
package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/matty-v/kyber/pkg/logging"
	"github.com/matty-v/kyber/pkg/runtimes"
)

type config struct {
	botToken, appToken, inboundURL, agentName, binding, hmacSecret string
	users, channels map[string]bool
	mcpAddr, healthAddr string
}

func csv(s string) map[string]bool { out:=map[string]bool{}; for _, v:=range strings.Split(s, ",") { if v=strings.TrimSpace(v); v!="" { out[v]=true } }; return out }
func loadConfig() (config,error) {
	c:=config{botToken:os.Getenv("SLACK_BOT_TOKEN"), appToken:os.Getenv("SLACK_APP_TOKEN"), inboundURL:os.Getenv("KYBER_INBOUND_URL"), agentName:os.Getenv("KYBER_AGENT_NAME"), binding:os.Getenv("KYBER_INBOUND_BINDING"), hmacSecret:os.Getenv("KYBER_INBOUND_HMAC_SECRET"), users:csv(os.Getenv("SLACK_ALLOWED_USER_IDS")), channels:csv(os.Getenv("SLACK_ALLOWED_CHANNEL_IDS")), mcpAddr:os.Getenv("KYBER_SLACK_MCP_ADDR"), healthAddr:os.Getenv("KYBER_SLACK_HEALTH_ADDR")}
	if c.binding=="" { c.binding="slack" }; if c.mcpAddr=="" { c.mcpAddr=runtimes.SlackMCPAddr() }; if c.healthAddr=="" { c.healthAddr=":14009" }
	if c.botToken=="" || c.appToken=="" || c.inboundURL=="" || c.agentName=="" { return c,fmt.Errorf("SLACK_BOT_TOKEN, SLACK_APP_TOKEN, KYBER_INBOUND_URL and KYBER_AGENT_NAME are required") }
	return c,nil
}

type slackEvent struct { Type string `json:"type"`; User string `json:"user"`; Channel string `json:"channel"`; Text string `json:"text"`; TS string `json:"ts"`; ThreadTS string `json:"thread_ts"`; Subtype string `json:"subtype"` }
type eventEnvelope struct { EnvelopeID string `json:"envelope_id"`; Type string `json:"type"`; Payload struct { Event slackEvent `json:"event"` } `json:"payload"` }

func sign(secret []byte, body []byte) string { h:=hmac.New(sha256.New,secret); _,_=h.Write(body); return "sha256="+hex.EncodeToString(h.Sum(nil)) }
func forward(ctx context.Context, c config, e eventEnvelope, client *http.Client) error {
	ev:=e.Payload.Event; if ev.Type!="message" || ev.User=="" || ev.Text=="" || ev.Channel=="" { return nil }; if len(c.users)==0 || !c.users[ev.User] || len(c.channels)==0 || !c.channels[ev.Channel] { return nil }
	b,_:=json.Marshal(map[string]any{"source":"slack","user":ev.User,"user_id":ev.User,"channel_id":ev.Channel,"message_id":ev.TS,"content":ev.Text,"thread_id":ev.ThreadTS})
	req,_:=http.NewRequestWithContext(ctx,http.MethodPost,strings.TrimRight(c.inboundURL,"/")+"/webhooks/inbound/"+c.agentName+"/"+c.binding,bytes.NewReader(b)); req.Header.Set("Content-Type","application/json"); if c.hmacSecret!="" { req.Header.Set("X-Kyber-Signature-256",sign([]byte(c.hmacSecret),b)) }
	r,err:=client.Do(req); if err!=nil{return err}; defer r.Body.Close(); io.Copy(io.Discard,r.Body); if r.StatusCode>=300{return fmt.Errorf("inbound returned %s",r.Status)}; return nil
}
func openSocket(ctx context.Context, token string) (string,error) { req,_:=http.NewRequestWithContext(ctx,http.MethodPost,"https://slack.com/api/apps.connections.open",nil); req.Header.Set("Authorization","Bearer "+token); req.Header.Set("Content-Type","application/x-www-form-urlencoded"); r,err:=http.DefaultClient.Do(req); if err!=nil{return "",err}; defer r.Body.Close(); var v struct{OK bool `json:"ok"`; URL string `json:"url"`; Error string `json:"error"`}; if err=json.NewDecoder(r.Body).Decode(&v); err!=nil{return "",err}; if !v.OK{return "",fmt.Errorf("Slack: %s",v.Error)}; return v.URL,nil }
func postMessage(ctx context.Context, token, channel, text, thread string) error { body:=map[string]string{"channel":channel,"text":text}; if thread!=""{body["thread_ts"]=thread}; b,_:=json.Marshal(body); req,_:=http.NewRequestWithContext(ctx,http.MethodPost,"https://slack.com/api/chat.postMessage",bytes.NewReader(b)); req.Header.Set("Authorization","Bearer "+token); req.Header.Set("Content-Type","application/json"); r,err:=http.DefaultClient.Do(req); if err!=nil{return err}; defer r.Body.Close(); var v struct{OK bool `json:"ok"`; Error string `json:"error"`}; json.NewDecoder(r.Body).Decode(&v); if !v.OK{return fmt.Errorf("Slack: %s",v.Error)}; return nil }

func main() {
	logger,err:=logging.New(logging.Config{Component:"slack-sidecar",Level:os.Getenv("KYBER_LOG_LEVEL")}); if err!=nil{fmt.Fprintln(os.Stderr,err);os.Exit(2)}; slog.SetDefault(logger); c,err:=loadConfig(); if err!=nil{slog.Error("slack-sidecar: bad config","error",err);os.Exit(2)}
	ctx,stop:=signal.NotifyContext(context.Background(),syscall.SIGINT,syscall.SIGTERM); defer stop(); client:=&http.Client{Timeout:15*time.Second}; srv:=newMCPServer(c,client); go http.ListenAndServe(c.mcpAddr,srv); go func(){ http.HandleFunc("/healthz",func(w http.ResponseWriter,r *http.Request){w.WriteHeader(http.StatusOK)}); _=http.ListenAndServe(c.healthAddr,nil) }()
	for ctx.Err()==nil { url,err:=openSocket(ctx,c.appToken); if err!=nil{slog.Warn("slack-sidecar: Socket Mode open failed","error",err); select{case <-ctx.Done():return;case <-time.After(5*time.Second):};continue}; conn,_,err:=websocket.DefaultDialer.Dial(url,nil); if err!=nil{slog.Warn("slack-sidecar: websocket failed","error",err);continue}; for { var msg eventEnvelope; if err:=conn.ReadJSON(&msg); err!=nil{conn.Close();break}; if msg.EnvelopeID!=""{_ = conn.WriteJSON(map[string]string{"envelope_id":msg.EnvelopeID})}; if err:=forward(ctx,c,msg,client); err!=nil{slog.Warn("slack-sidecar: forwarding failed","error",err)} }; }
}
