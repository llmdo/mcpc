package stdio

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mcpserver_for_test/mcps"
	"os"
	"strings"
)

/*
=========================

	STDIO
	=========================
*/
func StdioLoop() {
	reader := bufio.NewReader(os.Stdin)
	writer := bufio.NewWriter(os.Stdout)

	for {
		// 读取 Content-Length
		line, err := reader.ReadString('\n')
		if err != nil {
			log.Printf("stdio read error: %v", err)
			return
		}
		if !strings.HasPrefix(line, "Content-Length:") {
			continue
		}
		var length int
		fmt.Sscanf(line, "Content-Length: %d", &length)

		// 跳过空行
		_, _ = reader.ReadString('\n')

		// 读取消息体
		buf := make([]byte, length)
		_, err = io.ReadFull(reader, buf)
		if err != nil {
			log.Printf("stdio body read error: %v", err)
			return
		}

		// 解析 RPC 请求
		var req mcps.RpcReq
		if err := json.Unmarshal(buf, &req); err != nil {
			resp := mcps.RpcError(nil, -32700, "Parse error")
			writeResp(writer, resp)
			continue
		}

		resp := mcps.DispatchRPC(&req)
		writeResp(writer, resp)
	}
}

func writeResp(w *bufio.Writer, resp *mcps.RpcResp) {
	if resp == nil {
		return
	}
	b, _ := json.Marshal(resp)
	fmt.Fprintf(w, "Content-Length: %d\r\n\r\n", len(b))
	w.Write(b)
	w.Flush()
}
