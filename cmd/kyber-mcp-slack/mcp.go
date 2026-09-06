package main

import (
 "context"
 "encoding/json"
 "fmt"
 "net/http"
 "strings"
)
type mcpServer struct{ cfg config; client *http.Client }
func newMCPServer(c config,h *http.Client)*mcpServer{return &mcpServer{cfg:c,client:h}}
func (s *mcpServer) ServeHTTP(w http.ResponseWriter,r *http.Request){if r.Method!=http.MethodPost{http.Error(w,"method not allowed",405);return}; var q struct{JSONRPC string `json:"jsonrpc"`; ID json.RawMessage `json:"id"`; Method string `json:"method"`; Params json.RawMessage `json:"params"`}; if json.NewDecoder(http.MaxBytesReader(w,r.Body,1<<20)).Decode(&q)!=nil{return}; if len(q.ID)==0{w.WriteHeader(202);return}; out:=map[string]any{"jsonrpc":"2.0","id":q.ID}; switch q.Method{case "initialize":out["result"]=map[string]any{"protocolVersion":"2025-06-18","capabilities":map[string]any{"tools":map[string]any{}},"serverInfo":map[string]any{"name":"kyber-mcp-slack","version":"1"}}; case "ping":out["result"]=map[string]any{}; case "tools/list":out["result"]=map[string]any{"tools":[]any{map[string]any{"name":"reply","description":"Reply in Slack","inputSchema":map[string]any{"type":"object","properties":map[string]any{"channel_id":map[string]any{"type":"string"},"text":map[string]any{"type":"string"},"thread_ts":map[string]any{"type":"string"}},"required":[]string{"channel_id","text"}}}}}; case "tools/call":out["result"]=s.call(r.Context(),q.Params); default:out["error"]=map[string]any{"code":-32601,"message":"method not found"}}; w.Header().Set("Content-Type","application/json");json.NewEncoder(w).Encode(out)}
func (s *mcpServer) call(ctx context.Context,raw json.RawMessage)map[string]any{var p struct{Name string `json:"name"`;Arguments map[string]any `json:"arguments"`};if json.Unmarshal(raw,&p)!=nil||p.Name!="reply"{return map[string]any{"isError":true,"content":[]any{map[string]string{"type":"text","text":"unknown tool"}}}}; arg:=func(k string)string{v,_:=p.Arguments[k].(string);return strings.TrimSpace(v)}; ch,text,thread:=arg("channel_id"),arg("text"),arg("thread_ts");if ch==""||text==""{return map[string]any{"isError":true,"content":[]any{map[string]string{"type":"text","text":"channel_id and text are required"}}}};if err:=postMessage(ctx,s.cfg.botToken,ch,text,thread);err!=nil{return map[string]any{"isError":true,"content":[]any{map[string]string{"type":"text","text":fmt.Sprintf("Slack rejected the reply: %v",err)}}}};return map[string]any{"content":[]any{map[string]string{"type":"text","text":"reply sent"}}}}
