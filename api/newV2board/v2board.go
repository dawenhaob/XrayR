package newV2board

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/bitly/go-simplejson"
	"github.com/go-resty/resty/v2"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/infra/conf"

	"github.com/XrayR-project/XrayR/api"
)

// APIClient create an api client to the panel.
type APIClient struct {
	client           *resty.Client
	APIHost          string
	NodeID           int
	Key              string
	NodeType         string
	EnableVless      bool
	VlessFlow        string
	SpeedLimit       float64
	DeviceLimit      int
	LocalRuleList    []api.DetectRule
	LastReportOnline map[int]int
	resp             atomic.Value
	eTags            map[string]string
}

// New create an api instance
func New(apiConfig *api.Config) *APIClient {
	client := resty.New()
	client.SetRetryCount(3)
	if apiConfig.Timeout > 0 {
		client.SetTimeout(time.Duration(apiConfig.Timeout) * time.Second)
	} else {
		client.SetTimeout(5 * time.Second)
	}
	client.OnError(func(req *resty.Request, err error) {
		if v, ok := err.(*resty.ResponseError); ok {
			// v.Response contains the last response from the server
			// v.Err contains the original error
			log.Print(v.Err)
		}
	})
	client.SetBaseURL(apiConfig.APIHost)

	// Create Key for each requests
	nodeType_for_requests := func() string {
		if apiConfig.NodeType == "V2ray" && apiConfig.EnableVless {
			return "vless"
		} else {
			return apiConfig.NodeType
		}
	}()

	client.SetQueryParams(map[string]string{
		"node_id":   strconv.Itoa(apiConfig.NodeID),
		"node_type": strings.ToLower(nodeType_for_requests),
		"token":     apiConfig.Key,
	})
	// Read local rule list
	localRuleList := readLocalRuleList(apiConfig.RuleListPath)
	apiClient := &APIClient{
		client:        client,
		NodeID:        apiConfig.NodeID,
		Key:           apiConfig.Key,
		APIHost:       apiConfig.APIHost,
		NodeType:      apiConfig.NodeType,
		EnableVless:   apiConfig.EnableVless,
		VlessFlow:     apiConfig.VlessFlow,
		SpeedLimit:    apiConfig.SpeedLimit,
		DeviceLimit:   apiConfig.DeviceLimit,
		LocalRuleList: localRuleList,
		eTags:         make(map[string]string),
	}
	return apiClient
}

// readLocalRuleList reads the local rule list file
func readLocalRuleList(path string) (LocalRuleList []api.DetectRule) {
	LocalRuleList = make([]api.DetectRule, 0)

	if path != "" {
		// open the file
		file, err := os.Open(path)
		// handle errors while opening
		if err != nil {
			log.Printf("Error when opening file: %s", err)
			return LocalRuleList
		}
		defer file.Close()
		fileScanner := bufio.NewScanner(file)

		// read line by line
		for fileScanner.Scan() {
			LocalRuleList = append(LocalRuleList, api.DetectRule{
				ID:      -1,
				Pattern: regexp.MustCompile(fileScanner.Text()),
			})
		}
		// handle first encountered error while reading
		if err := fileScanner.Err(); err != nil {
			log.Fatalf("Error while reading file: %s", err)
			return
		}
	}

	return LocalRuleList
}

// Describe return a description of the client
func (c *APIClient) Describe() api.ClientInfo {
	return api.ClientInfo{APIHost: c.APIHost, NodeID: c.NodeID, Key: c.Key, NodeType: c.NodeType}
}

// Debug set the client debug for client
func (c *APIClient) Debug() {
	c.client.SetDebug(true)
}

func (c *APIClient) assembleURL(path string) string {
	return c.APIHost + path
}

func (c *APIClient) parseResponse(res *resty.Response, path string, err error) (*simplejson.Json, error) {
	if err != nil {
		return nil, fmt.Errorf("request %s failed: %v", c.assembleURL(path), err)
	}

	if res.StatusCode() > 399 {
		return nil, fmt.Errorf("request %s failed: %s, %v", c.assembleURL(path), res.String(), err)
	}

	rtn, err := simplejson.NewJson(res.Body())
	if err != nil {
		return nil, fmt.Errorf("ret %s invalid", res.String())
	}

	return rtn, nil
}

// GetNodeInfo will pull NodeInfo Config from panel
func (c *APIClient) GetNodeInfo() (nodeInfo *api.NodeInfo, err error) {
	server := new(serverConfig)
	path := "/api/v1/server/UniProxy/config"

	res, err := c.client.R().
		SetHeader("If-None-Match", c.eTags["node"]).
		ForceContentType("application/json").
		Get(path)

	// Etag identifier for a specific version of a resource. StatusCode = 304 means no changed
	if res.StatusCode() == 304 {
		return nil, errors.New(api.NodeNotModified)
	}
	// update etag
	if res.Header().Get("Etag") != "" && res.Header().Get("Etag") != c.eTags["node"] {
		c.eTags["node"] = res.Header().Get("Etag")
	}

	nodeInfoResp, err := c.parseResponse(res, path, err)
	if err != nil {
		return nil, err
	}
	b, _ := nodeInfoResp.Encode()
	json.Unmarshal(b, server)

	if server.ServerPort == 0 {
		return nil, errors.New("server port must > 0")
	}

	c.resp.Store(server)

	switch c.NodeType {
	case "V2ray", "Vmess", "Vless":
		nodeInfo, err = c.parseV2rayNodeResponse(server)
	case "Trojan":
		nodeInfo, err = c.parseTrojanNodeResponse(server)
	case "Shadowsocks":
		nodeInfo, err = c.parseSSNodeResponse(server)
	default:
		return nil, fmt.Errorf("unsupported node type: %s", c.NodeType)
	}

	if err != nil {
		return nil, fmt.Errorf("parse node info failed: %s, \nError: %v", res.String(), err)
	}

	api.PushInterval = server.BaseConfig.PushInterval
	api.PullInterval = server.BaseConfig.PullInterval
	return nodeInfo, nil
}

// GetUserList will pull user form panel
func (c *APIClient) GetUserList() (UserList *[]api.UserInfo, err error) {
	var users []*user
	path := "/api/v1/server/UniProxy/user"

	switch c.NodeType {
	case "V2ray", "Trojan", "Shadowsocks", "Vmess", "Vless":
		break
	default:
		return nil, fmt.Errorf("unsupported node type: %s", c.NodeType)
	}

	res, err := c.client.R().
		SetHeader("If-None-Match", c.eTags["users"]).
		ForceContentType("application/json").
		Get(path)

	// Etag identifier for a specific version of a resource. StatusCode = 304 means no changed
	if res.StatusCode() == 304 {
		return nil, errors.New(api.UserNotModified)
	}
	// update etag
	if res.Header().Get("Etag") != "" && res.Header().Get("Etag") != c.eTags["users"] {
		c.eTags["users"] = res.Header().Get("Etag")
	}

	usersResp, err := c.parseResponse(res, path, err)
	if err != nil {
		return nil, err
	}
	b, _ := usersResp.Get("users").Encode()
	json.Unmarshal(b, &users)
	if len(users) == 0 {
		return nil, errors.New("users is null")
	}

	var deviceLimit int = 0
	var userList []api.UserInfo
	for _, user := range users {
		u := api.UserInfo{
			UID:  user.Id,
			UUID: user.Uuid,
		}
		// Support 1.7.1 speed limit
		if c.SpeedLimit > 0 {
			u.SpeedLimit = uint64(c.SpeedLimit * 1000000 / 8)
		} else {
			u.SpeedLimit = uint64(user.SpeedLimit * 1000000 / 8)
		}
		//Prefer local config
		if c.DeviceLimit > 0 {
			deviceLimit = c.DeviceLimit
		} else {
			deviceLimit = user.DeviceLimit
		}

		u.DeviceLimit = deviceLimit
		u.Email = u.UUID + "@v2board.user"
		if c.NodeType == "Shadowsocks" {
			u.Passwd = u.UUID
		}

		userList = append(userList, u)
	}

	return &userList, nil
}

// GetIpsList will pull user form panel
func (c *APIClient) GetIpsList() error {
	var users []*aips
	path := "/api/v1/server/UniProxy/aips"

	switch c.NodeType {
	case "V2ray", "Trojan", "Shadowsocks", "Vmess", "Vless":
		break
	default:
		return fmt.Errorf("unsupported node type: %s", c.NodeType)
	}

	res, err := c.client.R().
		SetHeader("If-None-Match", c.eTags["users"]).
		ForceContentType("application/json").
		Get(path)

	// Etag identifier for a specific version of a resource. StatusCode = 304 means no changed
	if res.StatusCode() == 304 {
		return errors.New("AliveIPs same")
	}
	// update etag
	if res.Header().Get("Etag") != "" && res.Header().Get("Etag") != c.eTags["users"] {
		c.eTags["users"] = res.Header().Get("Etag")
	}

	usersResp, err := c.parseResponse(res, path, err)
	if err != nil {
		return err
	}
	b, _ := usersResp.Get("users").Encode()
	json.Unmarshal(b, &users)
	if len(users) == 0 {
		return errors.New("users is null")
	}
	api.UserAliveIPsMap = new(sync.Map)
	for _, user := range users {
		if len(user.AliveIPs) > 0 {
			api.UserAliveIPsMap.Store(user.Id, user.AliveIPs)
			log.Printf("GetIpsList: userid=%d, aliveips=%s, lastOnline=%d", user.Id, user.AliveIPs, c.LastReportOnline[user.Id])
		}
	}

	return nil
}

// ReportUserTraffic reports the user traffic
func (c *APIClient) ReportUserTraffic(userTraffic *[]api.UserTraffic) error {
	path := "/api/v1/server/UniProxy/push"

	// json structure: {uid1: [u, d], uid2: [u, d], uid1: [u, d], uid3: [u, d]}
	data := make(map[int][]int64, len(*userTraffic))
	for _, traffic := range *userTraffic {
		data[traffic.UID] = []int64{traffic.Upload, traffic.Download}
	}

	res, err := c.client.R().SetBody(data).ForceContentType("application/json").Post(path)
	_, err = c.parseResponse(res, path, err)
	if err != nil {
		return err
	}

	return nil
}

// GetNodeRule implements the API interface
func (c *APIClient) GetNodeRule() (*[]api.DetectRule, error) {
	routes := c.resp.Load().(*serverConfig).Routes

	ruleList := c.LocalRuleList

	for i := range routes {
		if routes[i].Action == "block" {
			ruleList = append(ruleList, api.DetectRule{
				ID:      i,
				Pattern: regexp.MustCompile(strings.Join(routes[i].Match, "|")),
			})
		}
	}

	return &ruleList, nil
}

// ReportNodeStatus implements the API interface
func (c *APIClient) ReportNodeStatus(nodeStatus *api.NodeStatus) (err error) {
	return nil
}

// ReportNodeOnlineUsers implements the API interface
func (c *APIClient) ReportNodeOnlineUsers(onlineUserList *[]api.OnlineUser) error {
	reportOnline := make(map[int]int)
	data := make(map[int][]string)
	for _, onlineuser := range *onlineUserList {
		// json structure: { UID1:["ip1","ip2"],UID2:["ip3","ip4"] }
		data[onlineuser.UID] = append(data[onlineuser.UID], onlineuser.IP)
		if onlineuser.IP != "" {
			reportOnline[onlineuser.UID]++
		}
	}
	c.LastReportOnline = reportOnline // Update LastReportOnline

	path := "/api/v1/server/UniProxy/alive"
	res, err := c.client.R().SetBody(data).ForceContentType("application/json").Post(path)
	_, err = c.parseResponse(res, path, err)
	// 面板无对应接口时先不报错
	if err != nil {
		return nil
	}

	return nil
}

// ReportIllegal implements the API interface
func (c *APIClient) ReportIllegal(detectResultList *[]api.DetectResult) error {
	return nil
}

// parseTrojanNodeResponse parse the response for the given nodeInfo format
func (c *APIClient) parseTrojanNodeResponse(s *serverConfig) (*api.NodeInfo, error) {
	var (
		host   string
		header json.RawMessage
	)
	transportProtocol := func() string {
		if s.Network == "" {
			return "tcp"
		} else {
			return s.Network
		}
	}()
	switch transportProtocol {
	case "ws":
		if s.NetworkSettings.Headers != nil {
			if httpHeader, err := s.NetworkSettings.Headers.MarshalJSON(); err != nil {
				return nil, err
			} else {
				b, _ := simplejson.NewJson(httpHeader)
				host = b.Get("Host").MustString()
			}
		}
	case "tcp":
		if s.NetworkSettings.Header != nil {
			if httpHeader, err := s.NetworkSettings.Header.MarshalJSON(); err != nil {
				return nil, err
			} else {
				header = httpHeader
			}
		}
	}
	// Create GeneralNodeInfo
	nodeInfo := &api.NodeInfo{
		NodeType:          c.NodeType,
		NodeID:            c.NodeID,
		Port:              uint32(s.ServerPort),
		TransportProtocol: transportProtocol,
		Path:              s.NetworkSettings.Path,
		EnableTLS:         true,
		Host:              host,
		Header:            header,
		ServiceName:       s.NetworkSettings.ServiceName,
		NameServerConfig:  s.parseDNSConfig(),
	}
	return nodeInfo, nil
}

// parseSSNodeResponse parse the response for the given nodeInfo format
func (c *APIClient) parseSSNodeResponse(s *serverConfig) (*api.NodeInfo, error) {
	var header json.RawMessage

	if s.Obfs == "http" {
		path := "/"
		if p := s.ObfsSettings.Path; p != "" {
			if strings.HasPrefix(p, "/") {
				path = p
			} else {
				path += p
			}
		}
		h := simplejson.New()
		h.Set("type", "http")
		h.SetPath([]string{"request", "path"}, path)
		header, _ = h.Encode()
	}
	// Create GeneralNodeInfo
	return &api.NodeInfo{
		NodeType:          c.NodeType,
		NodeID:            c.NodeID,
		Port:              uint32(s.ServerPort),
		TransportProtocol: "tcp",
		CypherMethod:      s.Cipher,
		ServerKey:         s.ServerKey, // shadowsocks2022 share key
		NameServerConfig:  s.parseDNSConfig(),
		Header:            header,
	}, nil
}

// parseV2rayNodeResponse parse the response for the given nodeInfo format
func (c *APIClient) parseV2rayNodeResponse(s *serverConfig) (*api.NodeInfo, error) {
	var (
		host          string
		header        json.RawMessage
		enableTLS     bool
		enableREALITY bool
		dest          string
	)
	if s.TlsSettings.Dest != "" {
		dest = s.TlsSettings.Dest
	} else {
		dest = s.TlsSettings.Sni
	}
	realityconfig := api.REALITYConfig{
		Dest:             dest + ":" + s.TlsSettings.ServerPort,
		ProxyProtocolVer: s.TlsSettings.Xver,
		ServerNames:      []string{s.TlsSettings.Sni},
		PrivateKey:       s.TlsSettings.PrivateKey,
		ShortIds:         []string{s.TlsSettings.ShortId},
	}
	switch s.Network {
	case "ws":
		if s.NetworkSettings.Headers != nil {
			if httpHeader, err := s.NetworkSettings.Headers.MarshalJSON(); err != nil {
				return nil, err
			} else {
				b, _ := simplejson.NewJson(httpHeader)
				host = b.Get("Host").MustString()
			}
		}
	case "tcp":
		if s.NetworkSettings.Header != nil {
			if httpHeader, err := s.NetworkSettings.Header.MarshalJSON(); err != nil {
				return nil, err
			} else {
				header = httpHeader
			}
		}
	case "httpupgrade", "xhttp":
		if s.NetworkSettings.Headers != nil {
			if httpHeaders, err := s.NetworkSettings.Headers.MarshalJSON(); err != nil {
				return nil, err
			} else {
				b, _ := simplejson.NewJson(httpHeaders)
				host = b.Get("Host").MustString()
			}
		}
		if s.NetworkSettings.Host != "" {
			host = s.NetworkSettings.Host
		}
	}

	if s.Tls != 0 {
		enableTLS = true
		if s.Tls == 2 {
			enableREALITY = true
		}
	}

	// Create GeneralNodeInfo
	return &api.NodeInfo{
		NodeType:          c.NodeType,
		NodeID:            c.NodeID,
		Port:              uint32(s.ServerPort),
		AlterID:           0,
		TransportProtocol: s.Network,
		EnableTLS:         enableTLS,
		Path:              s.NetworkSettings.Path,
		Host:              host,
		EnableVless:       c.EnableVless,
		VlessFlow:         s.VlessFlow,
		ServiceName:       s.NetworkSettings.ServiceName,
		Header:            header,
		EnableREALITY:     enableREALITY,
		REALITYConfig:     &realityconfig,
		NameServerConfig:  s.parseDNSConfig(),
	}, nil
}

func (s *serverConfig) parseDNSConfig() (nameServerList []*conf.NameServerConfig) {
	for i := range s.Routes {
		if s.Routes[i].Action == "dns" {
			nameServerList = append(nameServerList, &conf.NameServerConfig{
				Address: &conf.Address{Address: net.ParseAddress(s.Routes[i].ActionValue)},
				Domains: s.Routes[i].Match,
			})
		}
	}

	return
}
