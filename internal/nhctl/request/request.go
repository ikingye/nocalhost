/*
Copyright 2020 The Nocalhost Authors.
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package request

import (
	"encoding/base64"
	"fmt"
	"github.com/imroc/req"
	"github.com/pkg/errors"
	"net"
	"nocalhost/internal/nhctl/app"
	"nocalhost/internal/nhctl/coloredoutput"
	"nocalhost/internal/nhctl/syncthing/ports"
	"nocalhost/pkg/nhctl/log"
	"nocalhost/pkg/nhctl/tools"
	"os"
	"os/exec"
	"strconv"
)

const (
	LOGIN            = "/v1/login"
	CREATAPPLICATION = "/v1/application"
	CREATCLUSTER     = "/v1/cluster"
	CREATUSER        = "/v1/users"
	CREATEDEVSPACE   = "/v1/application/%d/create_space"
	UPDATEDEVSPACE   = "/v1/dev_space/%d"
)

type ApiRequest struct {
	Req                     *req.Req
	BaseUrl                 string
	AuthToken               string
	ApplicationId           int
	ClusterId               int
	UserId                  int
	KubeConfig              string
	KubeConfigRaw           string
	Kubectl                 string
	NameSpace               string
	InternalKubeConfigRaw   string
	DevSpaceId              int
	NocalhostWebPort        int
	InjectBatchUserTemplate string
	InjectBatchUserIds      []int
	PortForwardPortLocally  int
	EnablePortForward       bool
	portForwardCmd          *exec.Cmd
}

type MiniKubeCluster struct {
	ApiEndPoint MiniKube `yaml:"apiEndpoints"`
}

type MiniKube struct {
	MiniKubeDetail MiniKubeInfo `yaml:"minikube"`
}

type MiniKubeInfo struct {
	AdvertiseAddress string `yaml:"advertiseAddress"`
	BindPort         int    `yaml:"bindPort"`
}

type Response struct {
	Code    int                    `json:"code"`
	Message string                 `json:"message"`
	Data    map[string]interface{} `json:"data"`
}

type LoginRes struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    Token  `json:"data"`
}

type Token struct {
	Token string `json:"token"`
}

func NewReq(baseUrl, kubeConfig, kubectl, namespace string, nocalhostWebPort int) *ApiRequest {
	return &ApiRequest{
		Req:              req.New(),
		BaseUrl:          baseUrl,
		KubeConfig:       kubeConfig,
		Kubectl:          kubectl,
		NameSpace:        namespace,
		NocalhostWebPort: nocalhostWebPort,
	}
}

func (q *ApiRequest) CheckPortIsAvailable(port int) bool {
	return ports.IsPortAvailable("127.0.0.1", port)
}

// need to expose `kubectl port-forward service/nocalhost-web 65124:inits.port -n nocalhost`
// then login with endpoint
func (q *ApiRequest) ExposeService() *ApiRequest {
	q.GetAvailableRandomLocalPort()

	params := []string{
		"port-forward",
		"service/nocalhost-web",
		strconv.Itoa(q.PortForwardPortLocally) + ":" + strconv.Itoa(q.NocalhostWebPort),
		"-n",
		q.NameSpace,
		"--kubeconfig",
		q.KubeConfig,
	}
	cmd := exec.Command(q.Kubectl, params...)
	cmd.Stdout = os.Stdout
	err := cmd.Start()
	if err != nil {
		log.Fatalf("fail to port-forward expose nocalhost-web, err: %s", err)
	}

	baseUrl := "http://127.0.0.1:" + strconv.Itoa(q.PortForwardPortLocally)
	fmt.Printf("pid is %d, wait for port-forward... %s:%s \n", cmd.Process.Pid, strconv.Itoa(q.PortForwardPortLocally), strconv.Itoa(q.NocalhostWebPort))

	for {
		conn, _ := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(q.PortForwardPortLocally)), app.DefaultInitPortForwardTimeOut)
		if conn != nil {
			_ = conn.Close()
			break
		}
	}

	q.BaseUrl = baseUrl
	q.EnablePortForward = true
	q.portForwardCmd = cmd
	return q
}

func (q *ApiRequest) IdleThePortForwardIfNeeded() error {
	if q.portForwardCmd != nil {

		coloredoutput.Hint(
			"We found that your nocalhost-web endpoint can not access directly \n"+
				"So we port-forward the endpoint to local automatically \n"+
				"And if you kill this process the port-forward will be terminated \n"+
				"You can use the command \n"+
				" 	'%s' \n"+
				"  when you want to access the nocalhost-web next time",

			fmt.Sprintf("kubectl port-forward service/nocalhost-web :%d -n %s ", q.NocalhostWebPort, q.NameSpace),
		)

		// wait for port-forward
		err := q.portForwardCmd.Wait()
		if err != nil {
			return err
		}
	}
	return nil
}

func (q *ApiRequest) GetAvailableRandomLocalPort() *ApiRequest {
	localPorts, err := ports.GetAvailablePort()
	if err != nil {
		log.Fatalf("get localhost available port fail, err %s", err)
	}
	q.PortForwardPortLocally = localPorts
	return q
}

func (q *ApiRequest) UpdateDataBaseClusterUser() *ApiRequest {
	params := req.Param{
		"kubeconfig": base64.StdEncoding.EncodeToString([]byte(q.InternalKubeConfigRaw)),
	}
	header := req.Header{
		"Accept":        "application/json",
		"Authorization": "Bearer " + q.AuthToken,
	}
	url := fmt.Sprintf(q.BaseUrl+UPDATEDEVSPACE, q.DevSpaceId)
	r, err := q.Req.Put(url, header, req.BodyJSON(&params))
	if err != nil {
		log.Fatalf("init fail, update dev space fail, err: %s", err)
	}
	res := Response{}
	err = r.ToJSON(&res)
	if err != nil {
		log.Fatalf("init fail, update dev space fail, err: %s", err)
	}
	if res.Code != 0 {
		log.Fatalf("init fail, update dev space fail, err: %s", res.Message)
	}
	return q
}

func (q *ApiRequest) Login(email, password string) *ApiRequest {
	var err error = nil

	for i := 0; i < 3; i++ {
		err = q.tryLogin(email, password)
		log.Infof("Try login to the end point fail, retry %d..", i+1)
		if err == nil {
			return q
		}
	}

	log.Info("Try login to nocalhost-web fail, try port forward...")
	q.ExposeService()

	for i := 0; i < 3; i++ {
		errAfterPortForward := q.tryLogin(email, password)
		if errAfterPortForward == nil {
			return q
		}
	}

	log.Fatal(err)
	return nil
}

func (q *ApiRequest) tryLogin(email, password string) error {
	params := req.Param{
		"email":    email,
		"password": password,
	}
	r, err := q.Req.Post(q.BaseUrl+LOGIN, params)
	if err != nil {
		return errors.Errorf("init fail, request for login fail, err: %s", err)
	}
	res := LoginRes{}
	err = r.ToJSON(&res)
	if err != nil {
		return errors.Errorf("init fail, request for login fail, err: %s", err)
	}
	q.AuthToken = res.Data.Token
	return nil
}

func (q *ApiRequest) AddBookInfoApplication(context string) *ApiRequest {
	if context == "" {
		context = app.DefaultInitApplicationGithub
	}

	params := req.Param{
		"context": context,
		"status":  1,
	}
	header := req.Header{
		"Accept":        "application/json",
		"Authorization": "Bearer " + q.AuthToken,
	}
	r, err := q.Req.Post(q.BaseUrl+CREATAPPLICATION, header, req.BodyJSON(&params))
	if err != nil {
		log.Fatalf("init fail, add bookinfo application fail, err: %s", err)
	}
	res := Response{}
	err = r.ToJSON(&res)
	if err != nil {
		log.Fatalf("init fail, add bookinfo application fail, err: %s", err)
	}
	if res.Code != 0 {
		log.Fatalf("init fail, add bookinfo application fail, err: %s", res.Message)
	}
	applicationId := int(res.Data["id"].(float64))
	if err != nil {
		log.Fatalf("init fail, add bookinfo application fail, err: %s", err)
	}
	q.ApplicationId = applicationId
	fmt.Println("added bookinfo application")
	return q
}

//func (q *ApiRequest) GetClusterMasterNodeIp() *ApiRequest {
//	params := []string{
//		"get",
//		"nodes",
//		"-l",
//		"node-role.kubernetes.io/master=\"\"",
//		"-o",
//		"jsonpath='{range .items[*]}{.status.addresses[?(@.type==\"InternalIP\")].address} {.spec.podCIDR} {\"\\n\"}{end}'",
//		"--kubeconfig",
//		q.KubeConfig,
//	}
//	result, err := tools.ExecCommand(nil, true, q.Kubectl, params...)
//	if err != nil {
//		log.Fatalf("get minikube master ip fail, err: %s", err)
//	}
//	nodeIP := strings.Replace(result, "\n", "", -1)
//	nodeIP = strings.TrimSpace(nodeIP)
//	if nodeIP != "" {
//		q.MiniKubeMasterIP = nodeIP
//	}
//	return q
//}

// use "kubectl config view --minify --raw --flatten --kubeconfig " get current-context
func (q *ApiRequest) GetKubeConfig() *ApiRequest {
	// if use minikube, it should set 127.0.0.1 to real node ip, because in pod it can not call api server
	// use kubectl config view -o jsonpath='{.users[?(@.name == "minikube")].user.client-certificate}' --minify
	// if return not "", that means using minikube, then use kubectl get nodes
	params := []string{
		"config",
		"view",
		"--minify",
		"--raw",
		"--flatten",
		"--kubeconfig",
		q.KubeConfig,
	}
	result, err := tools.ExecCommand(nil, true, q.Kubectl, params...)
	if err != nil {
		log.Fatalf("get kubeconfig raw context fail, please check you --kubeconfig and kubeconfig file, err: %s", err)
	}
	q.KubeConfigRaw = result
	return q
}

func (q *ApiRequest) AddCluster() *ApiRequest {
	bKubeConfig := base64.StdEncoding.EncodeToString([]byte(q.KubeConfigRaw))
	params := req.Param{
		"kubeconfig": bKubeConfig,
		"name":       "auto_init_cluster",
	}
	header := req.Header{
		"Accept":        "application/json",
		"Authorization": "Bearer " + q.AuthToken,
	}
	r, err := q.Req.Post(q.BaseUrl+CREATCLUSTER, header, req.BodyJSON(&params))
	if err != nil {
		log.Fatalf("init fail, add cluster fail, err: %s", err)
	}
	res := Response{}
	err = r.ToJSON(&res)
	if res.Code != 0 {
		log.Fatalf("init fail, add cluster fail, err: %s", res.Message)
	}
	clusterId := int(res.Data["id"].(float64))
	kubeConfig := res.Data["kubeconfig"].(string)
	if err != nil {
		log.Fatalf("init fail, add bookinfo application fail, err: %s", err)
	}
	q.ClusterId = clusterId
	q.InternalKubeConfigRaw = kubeConfig
	fmt.Println("added cluster")
	return q
}

func (q *ApiRequest) AddUser(email, password, name string) *ApiRequest {
	params := req.Param{
		"email":            email,
		"name":             name,
		"password":         password,
		"status":           1,
		"confirm_password": password,
		"is_admin":         0,
	}
	header := req.Header{
		"Accept":        "application/json",
		"Authorization": "Bearer " + q.AuthToken,
	}
	r, err := q.Req.Post(q.BaseUrl+CREATUSER, header, req.BodyJSON(&params))
	if err != nil {
		log.Fatalf("init fail, add user fail, err: %s", err)
	}
	res := Response{}
	err = r.ToJSON(&res)
	if res.Code != 0 {
		log.Fatalf("init fail, add user fail, err: %s", res.Message)
	}
	userId := int(res.Data["id"].(float64))
	if err != nil {
		log.Fatalf("init fail, add bookinfo application fail, err: %s", err)
	}
	q.UserId = userId
	fmt.Println("added user")
	return q
}

func (q *ApiRequest) AddDevSpace() *ApiRequest {
	params := req.Param{
		"cluster_id": q.ClusterId,
		"cpu":        0,
		"memory":     0,
		"user_id":    q.UserId,
	}
	header := req.Header{
		"Accept":        "application/json",
		"Authorization": "Bearer " + q.AuthToken,
	}
	r, err := q.Req.Post(q.BaseUrl+fmt.Sprintf(CREATEDEVSPACE, q.ApplicationId), header, req.BodyJSON(&params))
	if err != nil {
		log.Fatalf("init fail, add dev space fail, err: %s", err)
	}
	res := Response{}
	err = r.ToJSON(&res)
	if res.Code != 0 {
		log.Fatalf("init fail, add dev space, err: %s", res.Message)
	}
	fmt.Println("added develop space")
	devSpaceId := int(res.Data["id"].(float64))
	kubeConfig := res.Data["kubeconfig"].(string)
	// TODO

	fmt.Printf("create dev space kubeconfig %s", kubeConfig)

	q.InternalKubeConfigRaw = kubeConfig
	q.DevSpaceId = devSpaceId
	return q
}

func (q *ApiRequest) SetInjectBatchUserTemplate(userTemplate string) *ApiRequest {
	q.InjectBatchUserTemplate = userTemplate
	return q
}

func (q *ApiRequest) InjectBatchDevSpace(amount, offset int) *ApiRequest {
	for i := offset; i < amount+offset; i++ {
		userName := fmt.Sprintf(q.InjectBatchUserTemplate+"@nocalhost.dev", i)
		name := fmt.Sprintf(q.InjectBatchUserTemplate, i)
		fmt.Printf("username %s", userName)
		q.AddUser(userName, app.DefaultInitAdminPassWord, name)
		q.AddDevSpace()
	}
	return q
}
