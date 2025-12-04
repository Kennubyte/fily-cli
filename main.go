package main

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"math/big"
	"net/http"
	"os"
	"regexp"
	"sync"
	"time"

	peer "github.com/muka/peerjs-go"
	"github.com/pion/webrtc/v3"
)

type iceServerCreds struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username"`
	Credential string   `json:"credential"`
}

type iceConfig struct {
	IceServers iceServerCreds `json:"iceServers"`
}

type chunkInfo struct {
	Data  [][]byte
	Count int
	Total int
}

type chunkAssembler struct {
	mu     sync.Mutex
	chunks map[int]*chunkInfo
}

func newChunkAssembler() *chunkAssembler {
	return &chunkAssembler{
		chunks: make(map[int]*chunkInfo),
	}
}

func (ca *chunkAssembler) addChunk(peerData int, n int, total int, data []byte) []byte {
	ca.mu.Lock()
	defer ca.mu.Unlock()

	info, exists := ca.chunks[peerData]
	if !exists {
		info = &chunkInfo{
			Data:  make([][]byte, total),
			Count: 0,
			Total: total,
		}
		ca.chunks[peerData] = info
	}

	if info.Data[n] == nil {
		info.Data[n] = make([]byte, len(data))
		copy(info.Data[n], data)
		info.Count++
	}

	if info.Count == info.Total {
		var complete bytes.Buffer
		for _, chunk := range info.Data {
			complete.Write(chunk)
		}
		delete(ca.chunks, peerData)
		return complete.Bytes()
	}

	return nil
}

type Message map[string]interface{}

func newMessage(msgType string) Message {
	return Message{"type": msgType}
}

func decodeBinaryPack(data []byte) (map[string]interface{}, error) {
	if len(data) == 0 {
		return nil, io.EOF
	}

	result := make(map[string]interface{})
	pos := 0

	firstByte := data[pos]
	pos++

	var mapSize int
	if firstByte >= 0x80 && firstByte <= 0x8f {
		mapSize = int(firstByte & 0x0f)
	} else if firstByte == 0xde {
		if len(data) < pos+2 {
			return nil, io.ErrUnexpectedEOF
		}
		mapSize = int(binary.BigEndian.Uint16(data[pos : pos+2]))
		pos += 2
	} else if firstByte == 0xdf {
		if len(data) < pos+4 {
			return nil, io.ErrUnexpectedEOF
		}
		mapSize = int(binary.BigEndian.Uint32(data[pos : pos+4]))
		pos += 4
	} else {
		return nil, io.ErrUnexpectedEOF
	}

	for i := 0; i < mapSize; i++ {
		key, newPos, err := readBinaryPackString(data, pos)
		if err != nil {
			return nil, err
		}
		pos = newPos

		value, newPos, err := readBinaryPackValue(data, pos)
		if err != nil {
			return nil, err
		}
		pos = newPos

		result[key] = value
	}

	return result, nil
}

func readBinaryPackString(data []byte, pos int) (string, int, error) {
	if pos >= len(data) {
		return "", pos, io.ErrUnexpectedEOF
	}

	firstByte := data[pos]
	pos++

	var strLen int
	if firstByte >= 0xb0 && firstByte <= 0xbf {
		strLen = int(firstByte & 0x0f)
	} else if firstByte == 0xd8 {
		if pos+2 > len(data) {
			return "", pos, io.ErrUnexpectedEOF
		}
		strLen = int(binary.BigEndian.Uint16(data[pos : pos+2]))
		pos += 2
	} else if firstByte == 0xd9 {
		if pos+4 > len(data) {
			return "", pos, io.ErrUnexpectedEOF
		}
		strLen = int(binary.BigEndian.Uint32(data[pos : pos+4]))
		pos += 4
	} else {
		return "", pos, io.ErrUnexpectedEOF
	}

	if pos+strLen > len(data) {
		return "", pos, io.ErrUnexpectedEOF
	}

	str := string(data[pos : pos+strLen])
	pos += strLen
	return str, pos, nil
}

func readBinaryPackRaw(data []byte, pos int) ([]byte, int, error) {
	if pos >= len(data) {
		return nil, pos, io.ErrUnexpectedEOF
	}

	firstByte := data[pos]
	pos++

	var rawLen int
	if firstByte >= 0xa0 && firstByte <= 0xaf {
		rawLen = int(firstByte & 0x0f)
	} else if firstByte == 0xda {
		if pos+2 > len(data) {
			return nil, pos, io.ErrUnexpectedEOF
		}
		rawLen = int(binary.BigEndian.Uint16(data[pos : pos+2]))
		pos += 2
	} else if firstByte == 0xdb {
		if pos+4 > len(data) {
			return nil, pos, io.ErrUnexpectedEOF
		}
		rawLen = int(binary.BigEndian.Uint32(data[pos : pos+4]))
		pos += 4
	} else {
		return nil, pos, io.ErrUnexpectedEOF
	}

	if pos+rawLen > len(data) {
		return nil, pos, io.ErrUnexpectedEOF
	}

	rawData := make([]byte, rawLen)
	copy(rawData, data[pos:pos+rawLen])
	pos += rawLen
	return rawData, pos, nil
}

func readBinaryPackValue(data []byte, pos int) (interface{}, int, error) {
	if pos >= len(data) {
		return nil, pos, io.ErrUnexpectedEOF
	}

	firstByte := data[pos]

	if (firstByte >= 0xb0 && firstByte <= 0xbf) || firstByte == 0xd8 || firstByte == 0xd9 {
		return readBinaryPackString(data, pos)
	}

	if (firstByte >= 0xa0 && firstByte <= 0xaf) || firstByte == 0xda || firstByte == 0xdb {
		return readBinaryPackRaw(data, pos)
	}

	if firstByte <= 0x7f {
		return int64(firstByte), pos + 1, nil
	}
	if firstByte >= 0xe0 {
		return int64(int8(firstByte)), pos + 1, nil
	}
	if firstByte == 0xcc {
		if pos+2 > len(data) {
			return nil, pos, io.ErrUnexpectedEOF
		}
		return int64(data[pos+1]), pos + 2, nil
	}
	if firstByte == 0xcd {
		if pos+3 > len(data) {
			return nil, pos, io.ErrUnexpectedEOF
		}
		return int64(binary.BigEndian.Uint16(data[pos+1 : pos+3])), pos + 3, nil
	}
	if firstByte == 0xce {
		if pos+5 > len(data) {
			return nil, pos, io.ErrUnexpectedEOF
		}
		return int64(binary.BigEndian.Uint32(data[pos+1 : pos+5])), pos + 5, nil
	}
	if firstByte == 0xcf {
		if pos+9 > len(data) {
			return nil, pos, io.ErrUnexpectedEOF
		}
		return int64(binary.BigEndian.Uint64(data[pos+1 : pos+9])), pos + 9, nil
	}

	if firstByte == 0xca {
		if pos+5 > len(data) {
			return nil, pos, io.ErrUnexpectedEOF
		}
		bits := binary.BigEndian.Uint32(data[pos+1 : pos+5])
		return float64(float32FromBits(bits)), pos + 5, nil
	}
	if firstByte == 0xcb {
		if pos+9 > len(data) {
			return nil, pos, io.ErrUnexpectedEOF
		}
		bits := binary.BigEndian.Uint64(data[pos+1 : pos+9])
		return float64FromBits(bits), pos + 9, nil
	}

	if firstByte == 0xc0 {
		return nil, pos + 1, nil
	}

	if firstByte == 0xc1 {
		return nil, pos + 1, nil
	}

	if firstByte == 0xc2 {
		return false, pos + 1, nil
	}
	if firstByte == 0xc3 {
		return true, pos + 1, nil
	}

	if firstByte >= 0x90 && firstByte <= 0x9f {
		arrayLen := int(firstByte & 0x0f)
		return readBinaryPackArray(data, pos+1, arrayLen)
	}
	if firstByte == 0xdc {
		if pos+3 > len(data) {
			return nil, pos, io.ErrUnexpectedEOF
		}
		arrayLen := int(binary.BigEndian.Uint16(data[pos+1 : pos+3]))
		return readBinaryPackArray(data, pos+3, arrayLen)
	}
	if firstByte == 0xdd {
		if pos+5 > len(data) {
			return nil, pos, io.ErrUnexpectedEOF
		}
		arrayLen := int(binary.BigEndian.Uint32(data[pos+1 : pos+5]))
		return readBinaryPackArray(data, pos+5, arrayLen)
	}

	if firstByte >= 0x80 && firstByte <= 0x8f {
		mapSize := int(firstByte & 0x0f)
		return readBinaryPackMap(data, pos+1, mapSize)
	}
	if firstByte == 0xde {
		if pos+3 > len(data) {
			return nil, pos, io.ErrUnexpectedEOF
		}
		mapSize := int(binary.BigEndian.Uint16(data[pos+1 : pos+3]))
		return readBinaryPackMap(data, pos+3, mapSize)
	}
	if firstByte == 0xdf {
		if pos+5 > len(data) {
			return nil, pos, io.ErrUnexpectedEOF
		}
		mapSize := int(binary.BigEndian.Uint32(data[pos+1 : pos+5]))
		return readBinaryPackMap(data, pos+5, mapSize)
	}

	return nil, pos, io.ErrUnexpectedEOF
}

func readBinaryPackArray(data []byte, pos int, length int) ([]interface{}, int, error) {
	result := make([]interface{}, length)
	for i := 0; i < length; i++ {
		val, newPos, err := readBinaryPackValue(data, pos)
		if err != nil {
			return nil, pos, err
		}
		result[i] = val
		pos = newPos
	}
	return result, pos, nil
}

func readBinaryPackMap(data []byte, pos int, mapSize int) (map[string]interface{}, int, error) {
	result := make(map[string]interface{})
	for i := 0; i < mapSize; i++ {
		key, newPos, err := readBinaryPackString(data, pos)
		if err != nil {
			return nil, pos, err
		}
		pos = newPos

		value, newPos, err := readBinaryPackValue(data, pos)
		if err != nil {
			return nil, pos, err
		}
		pos = newPos
		result[key] = value
	}
	return result, pos, nil
}

func float32FromBits(bits uint32) float32 {
	sign := bits >> 31
	exp := int((bits>>23)&0xff) - 127
	fraction := float64((bits & 0x7fffff) | 0x800000)
	if sign == 0 {
		return float32(fraction * pow2(float64(exp-23)))
	}
	return float32(-fraction * pow2(float64(exp-23)))
}

func float64FromBits(bits uint64) float64 {
	sign := bits >> 63
	exp := int((bits>>52)&0x7ff) - 1023
	hfrac := float64((bits & 0xfffff) | 0x100000)
	lfrac := float64(bits & 0xffffffff)
	frac := hfrac*pow2(float64(exp-20)) + lfrac*pow2(float64(exp-52))
	if sign == 0 {
		return frac
	}
	return -frac
}

func pow2(exp float64) float64 {
	if exp >= 0 {
		result := 1.0
		for i := 0; i < int(exp); i++ {
			result *= 2
		}
		return result
	}
	result := 1.0
	for i := 0; i > int(exp); i-- {
		result /= 2
	}
	return result
}

func encodeBinaryPack(data map[string]interface{}) []byte {
	var buf bytes.Buffer
	encodeBinaryPackMap(&buf, data)
	return buf.Bytes()
}

func encodeBinaryPackMap(buf *bytes.Buffer, data map[string]interface{}) {
	length := len(data)
	if length <= 0x0f {
		buf.WriteByte(byte(0x80 + length))
	} else if length <= 0xffff {
		buf.WriteByte(0xde)
		buf.WriteByte(byte(length >> 8))
		buf.WriteByte(byte(length & 0xff))
	} else {
		buf.WriteByte(0xdf)
		buf.WriteByte(byte(length >> 24))
		buf.WriteByte(byte(length >> 16))
		buf.WriteByte(byte(length >> 8))
		buf.WriteByte(byte(length & 0xff))
	}

	for key, value := range data {
		encodeBinaryPackString(buf, key)
		encodeBinaryPackValue(buf, value)
	}
}

func encodeBinaryPackString(buf *bytes.Buffer, s string) {
	strBytes := []byte(s)
	length := len(strBytes)
	if length <= 0x0f {
		buf.WriteByte(byte(0xb0 + length))
	} else if length <= 0xffff {
		buf.WriteByte(0xd8)
		buf.WriteByte(byte(length >> 8))
		buf.WriteByte(byte(length & 0xff))
	} else {
		buf.WriteByte(0xd9)
		buf.WriteByte(byte(length >> 24))
		buf.WriteByte(byte(length >> 16))
		buf.WriteByte(byte(length >> 8))
		buf.WriteByte(byte(length & 0xff))
	}
	buf.Write(strBytes)
}

func encodeBinaryPackRaw(buf *bytes.Buffer, data []byte) {
	length := len(data)
	if length <= 0x0f {
		buf.WriteByte(byte(0xa0 + length))
	} else if length <= 0xffff {
		buf.WriteByte(0xda)
		buf.WriteByte(byte(length >> 8))
		buf.WriteByte(byte(length & 0xff))
	} else {
		buf.WriteByte(0xdb)
		buf.WriteByte(byte(length >> 24))
		buf.WriteByte(byte(length >> 16))
		buf.WriteByte(byte(length >> 8))
		buf.WriteByte(byte(length & 0xff))
	}
	buf.Write(data)
}

func encodeBinaryPackValue(buf *bytes.Buffer, value interface{}) {
	switch v := value.(type) {
	case string:
		encodeBinaryPackString(buf, v)
	case int:
		encodeBinaryPackInteger(buf, int64(v))
	case int64:
		encodeBinaryPackInteger(buf, v)
	case float64:
		encodeBinaryPackDouble(buf, v)
	case bool:
		if v {
			buf.WriteByte(0xc3)
		} else {
			buf.WriteByte(0xc2)
		}
	case nil:
		buf.WriteByte(0xc0)
	case []byte:
		encodeBinaryPackRaw(buf, v)
	case map[string]interface{}:
		encodeBinaryPackMap(buf, v)
	case []interface{}:
		encodeBinaryPackArray(buf, v)
	}
}

func encodeBinaryPackInteger(buf *bytes.Buffer, num int64) {
	if num >= -32 && num <= 127 {
		buf.WriteByte(byte(num & 0xff))
	} else if num >= 0 && num <= 0xff {
		buf.WriteByte(0xcc)
		buf.WriteByte(byte(num))
	} else if num >= -128 && num <= 127 {
		buf.WriteByte(0xd0)
		buf.WriteByte(byte(num))
	} else if num >= 0 && num <= 0xffff {
		buf.WriteByte(0xcd)
		buf.WriteByte(byte(num >> 8))
		buf.WriteByte(byte(num & 0xff))
	} else if num >= -32768 && num <= 32767 {
		buf.WriteByte(0xd1)
		buf.WriteByte(byte(num >> 8))
		buf.WriteByte(byte(num & 0xff))
	} else if num >= 0 && num <= 0xffffffff {
		buf.WriteByte(0xce)
		buf.WriteByte(byte(num >> 24))
		buf.WriteByte(byte(num >> 16))
		buf.WriteByte(byte(num >> 8))
		buf.WriteByte(byte(num & 0xff))
	} else if num >= -2147483648 && num <= 2147483647 {
		buf.WriteByte(0xd2)
		buf.WriteByte(byte(num >> 24))
		buf.WriteByte(byte(num >> 16))
		buf.WriteByte(byte(num >> 8))
		buf.WriteByte(byte(num & 0xff))
	} else {
		buf.WriteByte(0xcf)
		buf.WriteByte(byte(num >> 56))
		buf.WriteByte(byte(num >> 48))
		buf.WriteByte(byte(num >> 40))
		buf.WriteByte(byte(num >> 32))
		buf.WriteByte(byte(num >> 24))
		buf.WriteByte(byte(num >> 16))
		buf.WriteByte(byte(num >> 8))
		buf.WriteByte(byte(num & 0xff))
	}
}

func encodeBinaryPackDouble(buf *bytes.Buffer, num float64) {
	buf.WriteByte(0xcb)
	bits := float64ToBits(num)
	buf.WriteByte(byte(bits >> 56))
	buf.WriteByte(byte(bits >> 48))
	buf.WriteByte(byte(bits >> 40))
	buf.WriteByte(byte(bits >> 32))
	buf.WriteByte(byte(bits >> 24))
	buf.WriteByte(byte(bits >> 16))
	buf.WriteByte(byte(bits >> 8))
	buf.WriteByte(byte(bits & 0xff))
}

func float64ToBits(f float64) uint64 {
	return math.Float64bits(f)
}

func encodeBinaryPackArray(buf *bytes.Buffer, arr []interface{}) {
	length := len(arr)
	if length <= 0x0f {
		buf.WriteByte(byte(0x90 + length))
	} else if length <= 0xffff {
		buf.WriteByte(0xdc)
		buf.WriteByte(byte(length >> 8))
		buf.WriteByte(byte(length & 0xff))
	} else {
		buf.WriteByte(0xdd)
		buf.WriteByte(byte(length >> 24))
		buf.WriteByte(byte(length >> 16))
		buf.WriteByte(byte(length >> 8))
		buf.WriteByte(byte(length & 0xff))
	}
	for _, item := range arr {
		encodeBinaryPackValue(buf, item)
	}
}

func getKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func getCreds() []webrtc.ICEServer {
	var creds iceConfig

	resp, err := http.Get("https://fily-credential-manager.kennubyte.workers.dev/")
	if err != nil {
		log.Fatalf("Failed to get credentials: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Fatalf("Failed to read response body: %v", err)
	}

	err = json.Unmarshal(body, &creds)
	if err != nil {
		log.Fatalf("Unmarshalling failed: %v", err)
	}

	iceServers := []webrtc.ICEServer{
		{
			URLs:       creds.IceServers.URLs,
			Username:   creds.IceServers.Username,
			Credential: creds.IceServers.Credential,
		},
	}
	return iceServers
}

func generateCode() string {
	max := big.NewInt(1000000)
	n, err := rand.Int(rand.Reader, max)
	if err != nil {
		log.Fatalf("Failed to generate code: %v", err)
	}
	return fmt.Sprintf("%06d", n.Int64())
}

func isCode(s string) bool {
	matched, _ := regexp.MatchString(`^\d{6}$`, s)
	return matched
}

func main() {
	args := os.Args[1:]

	if len(args) == 0 {
		log.Println("Usage: fily <file> or fily <6-digit-code>")
		return
	}

	arg := args[0]

	if isCode(arg) {
		connectToPeerAndReceiveFile(arg)
	} else {
		if _, err := os.Stat(arg); os.IsNotExist(err) {
			log.Fatalf("File not found: %s", arg)
		}
		code := generateCode()
		log.Printf("Share this code: %s", code)
		createPeerAndSendFile(code, arg)
	}
}

func createPeerAndSendFile(peerID, filePath string) {
	iceServers := getCreds()

	fileInfo, err := os.Stat(filePath)
	if err != nil {
		log.Fatalf("Failed to stat file: %v", err)
	}

	fileData, err := os.ReadFile(filePath)
	if err != nil {
		log.Fatalf("Failed to read file: %v", err)
	}

	opts := peer.NewOptions()
	opts.Configuration.ICEServers = iceServers
	peerInstance, err := peer.NewPeer(peerID+"-filyPeer-VWdOKQrqGPEtCm7sdiWmZAbtK", opts)
	if err != nil {
		log.Fatalf("Failed to create peer: %v", err)
	}

	peerInstance.On("open", func(id interface{}) {
		log.Printf("Peer ready with ID: %s", id)
	})

	peerInstance.On("error", func(err interface{}) {
		log.Printf("Peer error: %v", err)
	})

	peerInstance.On("connection", func(data interface{}) {
		conn := data.(*peer.DataConnection)
		log.Println("Connection established")

		conn.On("open", func(data interface{}) {})

		conn.On("data", func(data interface{}) {
			var msg map[string]interface{}

			switch v := data.(type) {
			case map[string]interface{}:
				msg = v
			case []byte:
				var err error
				msg, err = decodeBinaryPack(v)
				if err != nil {
					err = json.Unmarshal(v, &msg)
					if err != nil {
						return
					}
				}
			default:
				return
			}

			msgType, ok := msg["type"].(string)
			if !ok {
				return
			}

			switch msgType {
			case "requestFileData":
				log.Println("Sending file metadata...")
				responseMsg := map[string]interface{}{
					"type":     "responseFileData",
					"fileName": fileInfo.Name(),
					"fileSize": fileInfo.Size(),
				}
				conn.Send(encodeBinaryPack(responseMsg), true)

			case "requestFile":
				log.Println("Sending file...")
				chunkSize := 128 * 1024
				totalChunks := (len(fileData) + chunkSize - 1) / chunkSize
				chunkNum := 0
				for offset := 0; offset < len(fileData); offset += chunkSize {
					end := offset + chunkSize
					if end > len(fileData) {
						end = len(fileData)
					}

					chunk := fileData[offset:end]
					chunkMsg := map[string]interface{}{
						"type":     "responseFile",
						"file":     chunk,
						"fileSize": fileInfo.Size(),
						"fileName": fileInfo.Name(),
						"offset":   offset,
					}
					conn.Send(encodeBinaryPack(chunkMsg), true)
					chunkNum++
					log.Printf("Sent chunk %d/%d", chunkNum, totalChunks)
				}

				endMsg := map[string]interface{}{
					"type":     "responseFileEnd",
					"fileName": fileInfo.Name(),
				}
				conn.Send(encodeBinaryPack(endMsg), true)
				log.Println("File transfer completed")

				// Wait for data to be flushed before exiting
				time.Sleep(2 * time.Second)
				os.Exit(0)
			}
		})
	})

	log.Println("Waiting for connection...")
	select {}
}

func connectToPeerAndReceiveFile(peerID string) {
	iceServers := getCreds()

	opts := peer.NewOptions()
	opts.Configuration.ICEServers = iceServers
	peerInstance, err := peer.NewPeer("", opts)
	if err != nil {
		log.Fatalf("Failed to create peer: %v", err)
	}

	connOpts := peer.NewConnectionOptions()
	connOpts.Serialization = "binary"

	conn, err := peerInstance.Connect(peerID+"-filyPeer-VWdOKQrqGPEtCm7sdiWmZAbtK", connOpts)
	if err != nil {
		log.Fatalf("Failed to connect to peer: %v", err)
	}

	log.Println("Connecting to peer...")

	var outputFile *os.File
	var fileSize int64
	assembler := newChunkAssembler()

	conn.On("open", func(data interface{}) {
		log.Println("Connected, requesting file info...")
		reqFileData := map[string]interface{}{
			"type": "requestFileData",
		}
		conn.Send(encodeBinaryPack(reqFileData), true)
		// Don't request file here - wait for responseFileData first
	})

	conn.On("data", func(data interface{}) {
		var msg map[string]interface{}

		switch v := data.(type) {
		case map[string]interface{}:
			msg = v
		case []byte:
			var err error
			msg, err = decodeBinaryPack(v)
			if err != nil {
				err = json.Unmarshal(v, &msg)
				if err != nil {
					return
				}
			}
		default:
			return
		}

		if peerDataVal, hasPeerData := msg["__peerData"]; hasPeerData {
			if dataField, ok := msg["data"]; ok {
				if dataBytes, ok := dataField.([]byte); ok {
					var peerData, n, total int
					switch v := peerDataVal.(type) {
					case int64:
						peerData = int(v)
					case float64:
						peerData = int(v)
					}
					switch v := msg["n"].(type) {
					case int64:
						n = int(v)
					case float64:
						n = int(v)
					}
					switch v := msg["total"].(type) {
					case int64:
						total = int(v)
					case float64:
						total = int(v)
					}

					completeData := assembler.addChunk(peerData, n, total, dataBytes)
					if completeData == nil {
						return
					}

					var err error
					msg, err = decodeBinaryPack(completeData)
					if err != nil {
						return
					}
				} else {
					return
				}
			} else {
				return
			}
		}

		msgType, ok := msg["type"].(string)
		if !ok {
			return
		}

		switch msgType {
		case "responseFileData":
			fileName, _ := msg["fileName"].(string)
			switch v := msg["fileSize"].(type) {
			case int64:
				fileSize = v
			case float64:
				fileSize = int64(v)
			}

			// Get an available filename to avoid conflicts
			actualFileName := getAvailableFilename(fileName)
			if actualFileName != fileName {
				log.Printf("File %s exists, saving as %s", fileName, actualFileName)
			}
			log.Printf("Receiving: %s (%d bytes)", actualFileName, fileSize)

			var err error
			outputFile, err = os.Create(actualFileName)
			if err != nil {
				log.Fatalf("Failed to create output file: %v", err)
			}

			// Now request the actual file after output file is ready
			log.Println("Requesting file transfer...")
			reqFile := map[string]interface{}{
				"type": "requestFile",
			}
			conn.Send(encodeBinaryPack(reqFile), true)

		case "responseFile":
			if outputFile == nil {
				return
			}

			var fileData []byte
			if rawFile := msg["file"]; rawFile != nil {
				switch v := rawFile.(type) {
				case []byte:
					fileData = v
				case string:
					fileData = []byte(v)
				default:
					return
				}
			}

			_, err := outputFile.Write(fileData)
			if err != nil {
				log.Fatalf("Failed to write chunk: %v", err)
			}

			var offset int64
			switch v := msg["offset"].(type) {
			case int64:
				offset = v
			case float64:
				offset = int64(v)
			case uint64:
				offset = int64(v)
			case uint32:
				offset = int64(v)
			}
			percentage := (float64(offset+int64(len(fileData))) / float64(fileSize)) * 100
			log.Printf("Progress: %.1f%%", percentage)

		case "responseFileEnd":
			if outputFile != nil {
				outputFile.Close()
				fileName, _ := msg["fileName"].(string)
				log.Printf("Completed: %s", fileName)
				os.Exit(0)
			}
		}
	})

	select {}
}

func getAvailableFilename(fileName string) string {
	// Check if file exists or is in use
	if _, err := os.Stat(fileName); os.IsNotExist(err) {
		return fileName
	}

	// File exists, try adding a number suffix
	ext := ""
	base := fileName
	if idx := lastIndexByte(fileName, '.'); idx != -1 {
		ext = fileName[idx:]
		base = fileName[:idx]
	}

	for i := 1; i < 1000; i++ {
		newName := fmt.Sprintf("%s_%d%s", base, i, ext)
		if _, err := os.Stat(newName); os.IsNotExist(err) {
			return newName
		}
	}

	// Fallback: use timestamp
	return fmt.Sprintf("%s_%d%s", base, os.Getpid(), ext)
}

func lastIndexByte(s string, c byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == c {
			return i
		}
	}
	return -1
}
