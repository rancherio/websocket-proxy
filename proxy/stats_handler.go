package proxy

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	log "github.com/Sirupsen/logrus"
	jwt "github.com/dgrijalva/jwt-go"
	"github.com/gorilla/websocket"

	"github.com/rancher/websocket-proxy/common"
)

type StatsHandler struct {
	backend         backendProxy
	parsedPublicKey interface{}
}

type statsInfo struct {
	hostKey     string
	url         string
	msgKey      string
	respChannel <-chan common.Message
}

func (s *statsInfo) initializeClient(h *StatsHandler) error {
	if s.hostKey == "" {
		return fmt.Errorf("hostKey is empty")
	}
	msgKey, respChannel, err := h.backend.initializeClient(s.hostKey)
	if err != nil {
		return err
	}
	s.msgKey = msgKey
	s.respChannel = respChannel
	return nil
}

func (s *statsInfo) closeClient(h *StatsHandler) {
	h.backend.closeConnection(s.hostKey, s.msgKey)
}

func (s *statsInfo) connect(h *StatsHandler) error {
	return h.backend.connect(s.hostKey, s.msgKey, s.url)
}

func (h *StatsHandler) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	multiHost := false

	if strings.HasSuffix(req.URL.Path, "project") || strings.HasSuffix(req.URL.Path, "project/") || strings.HasSuffix(req.URL.Path, "service") || strings.HasSuffix(req.URL.Path, "service/") {
		multiHost = true
	}

	tokenString, authToken, err := h.auth(req)
	if err != nil {
		http.Error(rw, "Failed authentication", 401)
		return
	}

	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	ws, err := upgrader.Upgrade(rw, req, nil)
	if err != nil {
		http.Error(rw, "Failed to upgrade connection.", 500)
		return
	}

	if ok, _ := authToken.Claims["payload"].(bool); ok {
		ws.SetReadDeadline(time.Now().Add(30 * time.Second))
		_, content, err := ws.ReadMessage()
		if err != nil {
			http.Error(rw, "Failed to read payload", 500)
			return
		}
		tokenString = string(content)
	}

	statsInfoStructs, err := h.parseStatsInfo(req, tokenString, multiHost)
	if err != nil {
		log.Error("Failed to read statsinfo", err)
		http.Error(rw, "Failed to read payload targets", 500)
		return
	}

	var mutex sync.Mutex
	var countMutex sync.Mutex

	doneCounter := len(statsInfoStructs)

	defer func() {
		for _, statsInfoStruct := range statsInfoStructs {
			statsInfoStruct.closeClient(h)
		}
		closeConnection(ws)
	}()

	for _, statsInfoStruct := range statsInfoStructs {
		err := statsInfoStruct.initializeClient(h)
		if err != nil {
			return
		}

		// Send response messages to client
		go func(s *statsInfo) {
			errStatus := false
			for {
				message, ok := <-s.respChannel
				if !ok {
					return
				}
				switch message.Type {
				case common.Body:
					mutex.Lock()
					ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
					if err := ws.WriteMessage(1, []byte(message.Body)); err != nil {
						errStatus = true
					}
					mutex.Unlock()
				case common.Close:
					countMutex.Lock()
					errStatus = true
					doneCounter--
					countMutex.Unlock()
				}
				if errStatus && doneCounter == 0 {
					closeConnection(ws)
				}
			}
		}(statsInfoStruct)

		if err = statsInfoStruct.connect(h); err != nil {
			return
		}
	}

	ws.SetReadDeadline(time.Time{})
	for {
		_, _, err := ws.ReadMessage()
		if err != nil {
			return
		}
	}
}

func (h *StatsHandler) auth(req *http.Request) (string, *jwt.Token, error) {
	tokenString := req.URL.Query().Get("token")
	token, err := parseRequestToken(tokenString, h.parsedPublicKey)
	if err != nil {
		return "", nil, fmt.Errorf("Error parsing stats token. Failing auth. Error: %v", err)
	}

	if !token.Valid {
		return "", nil, fmt.Errorf("Token not valid")
	}

	return tokenString, token, nil
}

func (h *StatsHandler) parseStatsInfo(req *http.Request, tokenString string, multiHost bool) ([]*statsInfo, error) {
	token, err := parseRequestToken(tokenString, h.parsedPublicKey)
	if err != nil {
		return nil, fmt.Errorf("Error parsing stats token. Failing auth. Error: %v", err)
	}

	if !token.Valid {
		return nil, fmt.Errorf("Token not valid")
	}

	var statsInfoStructs []*statsInfo

	if multiHost {
		projectsOrServices, err := getProjectOrService(token)
		if err != nil {
			return nil, fmt.Errorf("Error getting project or service info from token %v", token)
		}
		for _, projectOrService := range projectsOrServices {
			data := projectOrService
			innerTokenString, ok := data["token"]
			if !ok {
				return nil, fmt.Errorf("Empty set of hosts or containers in project/service")
			}
			innerJwtToken, err := parseRequestToken(innerTokenString, h.parsedPublicKey)
			if err != nil {

				return nil, fmt.Errorf("Error getting inner token: %v. Inner token parameter: %v", err, innerTokenString)
			}
			hostUUID, found := h.extractHostUUID(innerJwtToken)
			if !found {
				return nil, fmt.Errorf("Couldn't find host uuid on inner token")
			}
			urlString, ok := data["url"]
			if !ok {
				return nil, fmt.Errorf("Could't find url field in inner token %v", data)
			}
			urlString = urlString + "?token=" + innerTokenString
			statsInfoStructs = append(statsInfoStructs, &statsInfo{hostKey: hostUUID, url: urlString})
		}
	} else {
		hostUUID, found := h.extractHostUUID(token)
		if !found {
			return nil, fmt.Errorf("could not find host uuid")
		}
		statsInfoStructs = append(statsInfoStructs, &statsInfo{hostKey: hostUUID, url: req.URL.String()})
	}
	return statsInfoStructs, nil
}

func getProjectOrService(token *jwt.Token) ([]map[string]string, error) {
	data, ok := token.Claims["project"]
	if !ok {
		data, ok = token.Claims["service"]
	}
	if ok {
		if interfaceList, isList := data.([]interface{}); isList {
			projectList := []map[string]string{}
			for _, inter := range interfaceList {
				projectInterfaceMap, ok := inter.(map[string]interface{})
				if ok {
					projectMap := map[string]string{}
					for key, value := range projectInterfaceMap {
						valueString, _ := value.(string)
						projectMap[key] = valueString
					}
					projectList = append(projectList, projectMap)
				} else {
					return nil, fmt.Errorf("invalid project/service input data type")
				}
			}
			return projectList, nil
		}
		return nil, fmt.Errorf("invalid project/service input data type")
	}
	return nil, fmt.Errorf("empty token")
}

func (h *StatsHandler) extractHostUUID(token *jwt.Token) (string, bool) {
	hostUUID, found := token.Claims["hostUuid"]
	if !found {
		log.WithFields(log.Fields{"hostUuid": hostUUID}).Infof("HostUuid not found in token.")
		return "", false
	}
	hostKey, ok := hostUUID.(string)
	if !ok || !h.backend.hasBackend(hostKey) {
		log.WithFields(log.Fields{"hostUuid": hostUUID}).Infof("Invalid HostUuid.")
		return "", false
	}
	return hostKey, true
}

func parseRequestToken(tokenString string, parsedPublicKey interface{}) (*jwt.Token, error) {
	if tokenString == "" {
		return nil, fmt.Errorf("No JWT provided")
	}

	token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
		return parsedPublicKey, nil
	})
	return token, err
}
