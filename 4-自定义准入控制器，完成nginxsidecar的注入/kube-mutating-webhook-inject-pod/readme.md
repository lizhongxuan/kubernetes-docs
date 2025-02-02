



# 编译打镜像
## makefile
```shell script
IMAGE_NAME ?= sidecar-injector

PWD := $(shell pwd)
BASE_DIR := $(shell basename $(PWD))

export GOPATH ?= $(GOPATH_DEFAULT)


IMAGE_TAG ?= $(shell date +v%Y%m%d)-$(shell git describe --match=$(git rev-parse --short=8 HEAD) --tags --always --dirty)

build:
	@echo "Building the $(IMAGE_NAME) binary..."
	@CGO_ENABLED=0 go build -o $(IMAGE_NAME) ./pkg/

build-linux:
	@echo "Building the $(IMAGE_NAME) binary for Docker (linux)..."
	@GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o $(IMAGE_NAME) ./pkg/

############################################################
# image section
############################################################

image: build-image

build-image: build-linux
	@echo "Building the docker image: $(IMAGE_NAME)..."
	@docker build -t $(IMAGE_NAME) -f Dockerfile .


.PHONY: all  build image

```

## dockerfile
```yaml
FROM alpine:latest

# set environment variables
ENV SIDECAR_INJECTOR=/usr/local/bin/sidecar-injector \
  USER_UID=1001 \
  USER_NAME=sidecar-injector

COPY sidecar-injector /usr/local/bin/sidecar-injector

# set entrypoint
ENTRYPOINT ["/usr/local/bin/sidecar-injector"]

# switch to non-root user
USER ${USER_UID}

```
## 打镜像
- 运行 make build-image
- 将镜像导出并传输到其他节点导入
```yaml
docker save  sidecar-injector > a.tar
scp a.tar k8s-node01:~ 
ctr --namespace k8s.io images import a.tar
```


# 部署
- 创建ns  nginx-injection，最终部署到这个ns中的容器会被注入nginx sidecar
```shell script
kubectl create ns nginx-injection

kubectl create ns sidecar-injector
```

- 创建ns  nginx-injection，最终部署到这个ns中的容器会被注入nginx sidecar
```shell script
kubectl create ns nginx-injection

kubectl create ns sidecar-injector
```


## 创建ca证书，并让apiserver签名

### 01 生成证书签名请求配置文件 csr.conf
```shell script
cat <<EOF > csr.conf
[req]
req_extensions = v3_req
distinguished_name = req_distinguished_name
[req_distinguished_name]
[ v3_req ]
basicConstraints = CA:FALSE
keyUsage = nonRepudiation, digitalSignature, keyEncipherment
extendedKeyUsage = serverAuth
subjectAltName = @alt_names
[alt_names]
DNS.1 = sidecar-injector-webhook-svc
DNS.2 = sidecar-injector-webhook-svc.sidecar-injector
DNS.3 = sidecar-injector-webhook-svc.sidecar-injector.svc
EOF
```
### 02 openssl genrsa 命生成RSA 私有秘钥
```shell script
openssl genrsa -out server-key.pem 2048
```


### 03 生成证书请求文件、验证证书请求文件和创建根CA
```shell script

openssl req -new -key server-key.pem -subj "/CN=sidecar-injector-webhook-svc.sidecar-injector.svc" -out server.csr -config csr.conf
```


## 删除之前的csr请求
```shell script
 kubectl delete csr sidecar-injector-webhook-svc.sidecar-injector
```

## 申请csr CertificateSigningRequest
```shell script


cat <<EOF | kubectl create -f -
apiVersion: certificates.k8s.io/v1beta1
kind: CertificateSigningRequest
metadata:
  name: sidecar-injector-webhook-svc.sidecar-injector
spec:
  groups:
  - system:authenticated
  request: $(< server.csr base64 | tr -d '\n')
  usages:
  - digital signature
  - key encipherment
  - server auth
EOF
```

- 检查csr
```shell script
[root@k8s-master01 ssl]# kubectl get csr 
NAME                                            AGE   SIGNERNAME                     REQUESTOR          CONDITION
sidecar-injector-webhook-svc.sidecar-injector   54s   kubernetes.io/legacy-unknown   kubernetes-admin   Pending
```

- 审批csr
```shell script
kubectl certificate approve sidecar-injector-webhook-svc.sidecar-injector
certificatesigningrequest.certificates.k8s.io/sidecar-injector-webhook-svc.sidecar-injector approved
```

## 获取签名后的证书
```shell script
serverCert=$(kubectl get csr sidecar-injector-webhook-svc.sidecar-injector -o jsonpath='{.status.certificate}')
echo "${serverCert}" | openssl base64 -d -A -out server-cert.pem

```
## 使用证书创建secret
```shell script
kubectl create secret generic sidecar-injector-webhook-certs \
        --from-file=key.pem=server-key.pem \
        --from-file=cert.pem=server-cert.pem \
        --dry-run=client -o yaml |
    kubectl -n sidecar-injector apply -f -
```
- 检查证书
```shell script
kubectl get secret -n  sidecar-injector
NAME                             TYPE                                  DATA   AGE
default-token-hvgnl              kubernetes.io/service-account-token   3      25m
sidecar-injector-webhook-certs   Opaque                                2      25m
```

## 获取CA_BUNDLE并替换 mutatingwebhook中的CA_BUNDLE占位符
```shell script
CA_BUNDLE=$(kubectl config view --raw --minify --flatten -o jsonpath='{.clusters[].cluster.certificate-authority-data}')

if [ -z "${CA_BUNDLE}" ]; then
    CA_BUNDLE=$(kubectl get secrets -o jsonpath="{.items[?(@.metadata.annotations['kubernetes\.io/service-account\.name']=='default')].data.ca\.crt}")
fi

```
- 替换
```shell script
cat deploy/mutating_webhook.yaml | sed -e "s|\${CA_BUNDLE}|${CA_BUNDLE}|g" >  deploy/mutatingwebhook-ca-bundle.yaml
```
- 检查结果 
```shell script
cat deploy/mutatingwebhook-ca-bundle.yaml
```

## 上述两个步骤可以直接运行脚本
- 脚本如下
```shell script
chmod +x ./deploy/*.sh
./deploy/webhook-create-signed-cert.sh \
    --service sidecar-injector-webhook-svc \
    --secret sidecar-injector-webhook-certs \
    --namespace sidecar-injector

cat deploy/mutating_webhook.yaml | \
    deploy/webhook-patch-ca-bundle.sh > \
    deploy/mutatingwebhook-ca-bundle.yaml
```
- 这里重用 Istio 项目中的生成的证书签名[请求脚本](https://github.com/istio/istio/blob/release-0.7/install/kubernetes/webhook-create-signed-cert.sh) 。通过发送请求到 apiserver，获取认证信息，然后使用获得的结果来创建需要的 secret 对象。

## 部署yaml
### 01 先部署sidecar-injector
- 部署
```shell script
kubectl create -f deploy/inject_configmap.yaml
kubectl create -f deploy/inject_deployment.yaml
kubectl create -f deploy/inject_service.yaml 
```
- 检查
```shell script
[root@k8s-master01 kube-mutating-webhook-inject-pod]# kubectl get pod -n sidecar-injector     

NAME                                                   READY   STATUS    RESTARTS   AGE
sidecar-injector-webhook-deployment-5cd7466c9f-xqpq4   1/1     Running   0          98s

[root@k8s-master01 kube-mutating-webhook-inject-pod]# kubectl get svc -n sidecar-injector   
NAME                           TYPE        CLUSTER-IP      EXTERNAL-IP   PORT(S)   AGE
sidecar-injector-webhook-svc   ClusterIP   10.96.171.114   <none>        443/TCP   111s
```

### 02 部署 mutatingwebhook
```shell script
kubectl create -f deploy/mutatingwebhook-ca-bundle.yaml
```

- 检查
```shell script
 kubectl get MutatingWebhookConfiguration -A   
NAME                           WEBHOOKS   AGE
sidecar-injector-webhook-cfg   1          57s
```

### 03 部署 nginx-sidecar 运行所需的configmap
```shell script
kubectl create -f deploy/nginx_configmap.yaml 

```

### 04 创建一个namespace ，并打上标签 sidecar-injection=enabled

```shell script
kubectl create ns nginx-injection

kubectl label namespace nginx-injection sidecar-injection=enabled

``` 
- sidecar-injection=enabled 和 MutatingWebhookConfiguration中的 ns过滤器相同
```yaml
  namespaceSelector:
    matchLabels:
      nginx-sidecar-injection: enabled

```
- 检查标签结果，最终部署到这里的pod都判断是否要注入sidecar
```yaml
kubectl get ns -L nginx-sidecar-injection
NAME               STATUS   AGE     NGINX-SIDECAR-INJECTION
calico-system      Active   156d    
default            Active   156d    
ingress-nginx      Active   91d     
injection          Active   24h     
kube-admin         Active   156d    
kube-node-lease    Active   156d    
kube-public        Active   156d    
kube-system        Active   156d    
monitoring         Active   4d4h    
nginx-injection    Active   3h57m   enabled
sidecar-injector   Active   4h38m   
tigera-operator    Active   156d  
```

### 05 向nginx-injection中部署一个pod
- annotations中配置的sidecar-injector-webhook.nginx.sidecar/need_inject: "true"代表需要注入
```yaml
apiVersion: v1
kind: Pod
metadata:
  namespace: nginx-injection
  name: test-alpine-inject01
  labels:
    role: myrole
  annotations:
    sidecar-injector-webhook.nginx.sidecar/need_inject: "true"
spec:
  containers:
    - image: alpine
      command:
        - /bin/sh
        - "-c"
        - "sleep 60m"
      imagePullPolicy: IfNotPresent
      name: alpine
  restartPolicy: Always
```
- 部署
```shell script
[root@k8s-master01 deploy]# kubectl create -f test_sleep_deployment.yaml
pod/test-alpine-inject-01 created
```


- 查看结果，可以看到test-alpine-inject-01 pod中被注入了nginx sidecar，curl这个pod的ip访问80端口，可以看到nginx sidecar的响应
```shell script
[root@k8s-master01 deploy]# kubectl get pod -n nginx-injection  -o wide 
NAME                    READY   STATUS    RESTARTS   AGE   IP              NODE         NOMINATED NODE   READINESS GATES
test-alpine-inject-01   2/2     Running   0          78s   10.100.85.216   k8s-node01   <none>           <none>
[root@k8s-master01 deploy]# curl 10.100.85.216
<html>
<head><title>404 Not Found</title></head>
<body bgcolor="white">
<center><h1>404 Not Found</h1></center>
<hr><center>nginx/1.12.2</center>
</body>
</html>
```

### 06 观察sidecar-injector的日志
- apiserver过来访问 sidecar-injector，然后经过判断后给改pod 注入了sidecar
```shell script

I0910 08:35:14.788857       1 webhook.go:179] serveMutate.receive.request: Body={"kind":"AdmissionReview","apiVersion":"admission.k8s.io/v1beta1","request":{"uid":"17d2ced2-38a1-4276-b98b-22fcc1eb1fae","kind":{"group":"","version":"v1","kind":"Pod"},"resource":{"group":"","version":"v1","resource":"pods"},"requestKind":{"group":"","version":"v1","kind":"Pod"},"requestResource":{"group":"","version":"v1","resource":"pods"},"name":"test-alpine-inject-01","namespace":"nginx-injection","operation":"CREATE","userInfo":{"username":"kubernetes-admin","groups":["system:masters","system:authenticated"]},"object":{"kind":"Pod","apiVersion":"v1","metadata":{"name":"test-alpine-inject-01","namespace":"nginx-injection","creationTimestamp":null,"labels":{"role":"myrole"},"annotations":{"sidecar-injector-webhook.nginx.sidecar/need_inject":"true"},"managedFields":[{"manager":"kubectl-create","operation":"Update","apiVersion":"v1","time":"2021-09-10T08:35:14Z","fieldsType":"FieldsV1","fieldsV1":{"f:metadata":{"f:annotations":{".":{},"f:sidecar-injector-webhook.nginx.sidecar/need_inject":{}},"f:labels":{".":{},"f:role":{}}},"f:spec":{"f:containers":{"k:{\"name\":\"alpine\"}":{".":{},"f:command":{},"f:image":{},"f:imagePullPolicy":{},"f:name":{},"f:resources":{},"f:terminationMessagePath":{},"f:terminationMessagePolicy":{}}},"f:dnsPolicy":{},"f:enableServiceLinks":{},"f:restartPolicy":{},"f:schedulerName":{},"f:securityContext":{},"f:terminationGracePeriodSeconds":{}}}}]},"spec":{"volumes":[{"name":"default-token-xk75k","secret":{"secretName":"default-token-xk75k"}}],"containers":[{"name":"alpine","image":"alpine","command":["/bin/sh","-c","sleep 60m"],"resources":{},"volumeMounts":[{"name":"default-token-xk75k","readOnly":true,"mountPath":"/var/run/secrets/kubernetes.io/serviceaccount"}],"terminationMessagePath":"/dev/termination-log","terminationMessagePolicy":"File","imagePullPolicy":"IfNotPresent"}],"restartPolicy":"Always","terminationGracePeriodSeconds":30,"dnsPolicy":"ClusterFirst","serviceAccountName":"default","serviceAccount":"default","securityContext":{},"schedulerName":"default-scheduler","tolerations":[{"key":"node.kubernetes.io/not-ready","operator":"Exists","effect":"NoExecute","tolerationSeconds":300},{"key":"node.kubernetes.io/unreachable","operator":"Exists","effect":"NoExecute","tolerationSeconds":300}],"priority":0,"enableServiceLinks":true,"preemptionPolicy":"PreemptLowerPriority"},"status":{}},"oldObject":null,"dryRun":false,"options":{"kind":"CreateOptions","apiVersion":"meta.k8s.io/v1","fieldManager":"kubectl-create"}}}

I0910 08:35:14.789561       1 webhook.go:128] AdmissionReview for Kind=/v1, Kind=Pod, Namespace=nginx-injection Name=test-alpine-inject-01 (test-alpine-inject-01) UID=17d2ced2-38a1-4276-b98b-22fcc1eb1fae patchOperation=CREATE UserInfo={kubernetes-admin  [system:masters system:authenticated] map[]}
I0910 08:35:14.789844       1 webhook.go:260] [addContainer.value][add:+v][value:+v] %!(EXTRA v1.Container={sidecar-nginx nginx:1.12.2 [] []  [{ 0 80  }] [] [] {map[] map[]} [{nginx-conf false /etc/nginx  <nil> }] [] nil nil nil nil   IfNotPresent nil false false false}, v1.Container={sidecar-nginx nginx:1.12.2 [] []  [{ 0 80  }] [] [] {map[] map[]} [{nginx-conf false /etc/nginx  <nil> }] [] nil nil nil nil   IfNotPresent nil false false false})
I0910 08:35:14.789945       1 webhook.go:154] AdmissionResponse: patch=[{"op":"add","path":"/spec/containers/-","value":{"name":"sidecar-nginx","image":"nginx:1.12.2","ports":[{"containerPort":80}],"resources":{},"volumeMounts":[{"name":"nginx-conf","mountPath":"/etc/nginx"}],"imagePullPolicy":"IfNotPresent"}},{"op":"add","path":"/spec/volumes/-","value":{"name":"nginx-conf","configMap":{"name":"nginx-configmap"}}},{"op":"add","path":"/metadata/annotations","value":{"sidecar-injector-webhook.nginx.sidecar/status":"injected"}}]
I0910 08:35:14.789987       1 webhook.go:221] Ready to write reponse ...

```

### 07 部署一个不需要注入sidecar的pod
- sidecar-injector-webhook.nginx.sidecar/need_inject: "false" 明确指出不需要如注入
```yaml
apiVersion: v1
kind: Pod
metadata:
  namespace: nginx-injection
  name: test-alpine-inject-02
  labels:
    role: myrole
  annotations:
    sidecar-injector-webhook.nginx.sidecar/need_inject: "false"
spec:
  containers:
    - image: alpine
      command:
        - /bin/sh
        - "-c"
        - "sleep 60m"
      imagePullPolicy: IfNotPresent
      name: alpine
  restartPolicy: Always
```
- 观察部署结果 ，test-alpine-inject-02 只运行了一个容器
```yaml
kubectl get pod -n nginx-injection  -o wide       
NAME                    READY   STATUS    RESTARTS   AGE    IP              NODE         NOMINATED NODE   READINESS GATES
test-alpine-inject-01   2/2     Running   0          6m8s   10.100.85.216   k8s-node01   <none>           <none>
test-alpine-inject-02   1/1     Running   0          77s    10.100.85.215   k8s-node01   <none>           <none>
```
- 观察sidecar-injector的日志 ，可以看到 [skip_mutation][reason=pod_not_need]
```shell script
I0910 08:40:05.466685       1 webhook.go:128] AdmissionReview for Kind=/v1, Kind=Pod, Namespace=nginx-injection Name=test-alpine-inject-02 (test-alpine-inject-02) UID=846d1f11-7793-4226-8e96-25a08ea21dff patchOperation=CREATE UserInfo={kubernetes-admin  [system:masters system:authenticated] map[]}
I0910 08:40:05.466733       1 webhook.go:106] [skip_mutation][reason=pod_not_need][name:test-alpine-inject-02][ns:nginx-injection]
I0910 08:40:05.466765       1 webhook.go:133] Skipping mutation for nginx-injection/test-alpine-inject-02 due to policy check
I0910 08:40:05.466815       1 webhook.go:221] Ready to write reponse ...

```
