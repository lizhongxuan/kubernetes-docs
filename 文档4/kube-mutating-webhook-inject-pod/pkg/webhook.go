package main

import (
	"encoding/json"
	"fmt"
	"github.com/ghodss/yaml"
	"github.com/golang/glog"
	"io/ioutil"
	"k8s.io/api/admission/v1beta1"
	admissionregistrationv1beta1 "k8s.io/api/admissionregistration/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"net/http"
	"strings"
)

var (
	runtimeScheme = runtime.NewScheme()
	codecs        = serializer.NewCodecFactory(runtimeScheme)
	deserializer  = codecs.UniversalDeserializer()

	// (https://github.com/kubernetes/kubernetes/issues/57982)
	defaulter = runtime.ObjectDefaulter(runtimeScheme)
)

const (
	// 代表这个pod是否要注入  = ture代表要注入
	admissionWebhookAnnotationInjectKey = "sidecar-injector-webhook.nginx.sidecar/need_inject"
	// 代表判断pod已经注入过的标志 = injected代表已经注入了，就不再注入
	admissionWebhookAnnotationStatusKey = "sidecar-injector-webhook.nginx.sidecar/status"
)

// 为了安全，不给这两个ns中的pod注入 sidecar
var ignoredNamespaces = []string{
	metav1.NamespaceSystem,
	metav1.NamespacePublic,
}

type Config struct {
	Containers []corev1.Container `yaml:"containers"`
	Volumes    []corev1.Volume    `yaml:"volumes"`
}

// Webhook Server options
type webHookSvrOptions struct {
	port           int    // 监听https的端口
	certFile       string // https x509 证书路径
	keyFile        string // https x509 证书私钥路径
	sidecarCfgFile string // 注入sidecar容器的配置文件路径
}

type webhookServer struct {
	sidecarConfig *Config      // 注入sidecar容器的配置
	server        *http.Server // http serer
}

func init() {
	_ = corev1.AddToScheme(runtimeScheme)
	_ = admissionregistrationv1beta1.AddToScheme(runtimeScheme)
	// defaulting with webhooks:
	// https://github.com/kubernetes/kubernetes/issues/57982
	//_ = v1.AddToScheme(runtimeScheme)
}

// (https://github.com/kubernetes/kubernetes/issues/57982)
func applyDefaultsWorkaround(containers []corev1.Container, volumes []corev1.Volume) {
	defaulter.Default(&corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: containers,
			Volumes:    volumes,
		},
	})
}

// 判断这个pod资源要不要注入
// 1. 如果pod在高权限的ns中，不注入
// 2. 如果pod annotations中 标记为已注入就不再注入了
// 3. 如果pod annotations中 配置不愿意注入就不注入
func mutationRequired(ignoredList []string, metadata *metav1.ObjectMeta) bool {
	// skip special kubernete system namespaces
	for _, namespace := range ignoredList {
		if metadata.Namespace == namespace {
			glog.Infof("[skip_mutation][reason=special_namespace][name:%v][ns:%v]", metadata.Name, metadata.Namespace)

			return false
		}
	}

	annotations := metadata.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}

	// 如果 annotation中 标记为已注入就不再注入了
	status := annotations[admissionWebhookAnnotationStatusKey]
	if strings.ToLower(status) == "injected" {
		glog.Infof("[skip_mutation][reason=already_injected][name:%v][ns:%v]", metadata.Name, metadata.Namespace)

		return false
	}
	// 如果pod中配置不愿意注入就不注入
	switch strings.ToLower(annotations[admissionWebhookAnnotationInjectKey]) {
	default:
		glog.Infof("[skip_mutation][reason=pod_not_need][name:%v][ns:%v]", metadata.Name, metadata.Namespace)

		return false
	case "true":
		return true
	}
}

func (ws *webhookServer) mutatePod(ar *v1beta1.AdmissionReview) *v1beta1.AdmissionResponse {
	// 将请求中的对象解析为pod，如果出错就返回

	req := ar.Request
	var pod corev1.Pod
	if err := json.Unmarshal(req.Object.Raw, &pod); err != nil {
		glog.Errorf("Could not unmarshal raw object: %v", err)
		return &v1beta1.AdmissionResponse{
			Result: &metav1.Status{
				Message: err.Error(),
			},
		}
	}

	glog.Infof("AdmissionReview for Kind=%v, Namespace=%v Name=%v (%v) UID=%v patchOperation=%v UserInfo=%v",
		req.Kind, req.Namespace, req.Name, pod.Name, req.UID, req.Operation, req.UserInfo)

	// 是否需要注入判断
	if !mutationRequired(ignoredNamespaces, &pod.ObjectMeta) {
		glog.Infof("Skipping mutation for %s/%s due to policy check", pod.Namespace, pod.Name)
		return &v1beta1.AdmissionResponse{
			Allowed: true,
		}
	}

	// Workaround: https://github.com/kubernetes/kubernetes/issues/57982
	glog.Infof("[before_applyDefaultsWorkaround][ws.sidecarConfig.Containers:%+v][ws.sidecarConfig.Volumes:%+v]", ws.sidecarConfig.Containers[0], ws.sidecarConfig.Volumes[0])
	applyDefaultsWorkaround(ws.sidecarConfig.Containers, ws.sidecarConfig.Volumes)
	glog.Infof("[after_applyDefaultsWorkaround][ws.sidecarConfig.Containers:%+v][ws.sidecarConfig.Volumes:%+v]", ws.sidecarConfig.Containers[0], ws.sidecarConfig.Volumes[0])

	annotations := map[string]string{admissionWebhookAnnotationStatusKey: "injected"}
	patchBytes, err := createPatch(&pod, ws.sidecarConfig, annotations)
	if err != nil {
		return &v1beta1.AdmissionResponse{
			Result: &metav1.Status{
				Message: err.Error(),
			},
		}
	}

	glog.Infof("AdmissionResponse: patch=%v\n", string(patchBytes))
	return &v1beta1.AdmissionResponse{
		Allowed: true,
		Patch:   patchBytes,
		PatchType: func() *v1beta1.PatchType {
			pt := v1beta1.PatchTypeJSONPatch
			return &pt
		}(),
	}
}

func (ws *webhookServer) serveMutate(w http.ResponseWriter, r *http.Request) {

	var body []byte
	if r.Body != nil {
		if data, err := ioutil.ReadAll(r.Body); err == nil {
			body = data
		}
	}
	if len(body) == 0 {
		glog.Error("empty body")
		http.Error(w, "empty body", http.StatusBadRequest)
		return
	}

	glog.Infof("serveMutate.receive.request: Body=%v\n", string(body))
	// verify the content type is accurate
	contentType := r.Header.Get("Content-Type")
	if contentType != "application/json" {
		glog.Errorf("Content-Type=%s, expect application/json", contentType)
		http.Error(w, "invalid Content-Type, expect `application/json`", http.StatusUnsupportedMediaType)
		return
	}
	// 构造准入控制器的响应
	var admissionResponse *v1beta1.AdmissionResponse
	// 构造准入控制的审查对象 包括请求和响应
	// 然后使用UniversalDeserializer解析传入的申请
	// 如果出错就设置响应为报错的信息
	// 没出错就调用mutatePod生成响应
	ar := v1beta1.AdmissionReview{}
	if _, _, err := deserializer.Decode(body, nil, &ar); err != nil {
		glog.Errorf("Can't decode body: %v", err)
		admissionResponse = &v1beta1.AdmissionResponse{
			Result: &metav1.Status{
				Message: err.Error(),
			},
		}
	} else {
		admissionResponse = ws.mutatePod(&ar)
	}

	// 构造最终响应对象 admissionReview
	// 给response赋值
	// json解析后用 w.write写入
	admissionReview := v1beta1.AdmissionReview{}
	if admissionResponse != nil {
		admissionReview.Response = admissionResponse
		if ar.Request != nil {
			admissionReview.Response.UID = ar.Request.UID
		}
	}

	resp, err := json.Marshal(admissionReview)
	if err != nil {
		glog.Errorf("Can't encode response: %v", err)
		http.Error(w, fmt.Sprintf("could not encode response: %v", err), http.StatusInternalServerError)
	}
	glog.Infof("Ready to write reponse ...")
	if _, err := w.Write(resp); err != nil {
		glog.Errorf("Can't write response: %v", err)
		http.Error(w, fmt.Sprintf("could not write response: %v", err), http.StatusInternalServerError)
	}

}

type patchOperation struct {
	Op    string      `json:"op"`              // 动作
	Path  string      `json:"path"`            // 操作的path
	Value interface{} `json:"value,omitempty"` // 值
}

// create mutation patch for resoures
func createPatch(pod *corev1.Pod, sidecarConfig *Config, annotations map[string]string) ([]byte, error) {
	var patch []patchOperation

	patch = append(patch, addContainer(pod.Spec.Containers, sidecarConfig.Containers, "/spec/containers")...)
	patch = append(patch, addVolume(pod.Spec.Volumes, sidecarConfig.Volumes, "/spec/volumes")...)
	patch = append(patch, updateAnnotation(pod.Annotations, annotations)...)

	return json.Marshal(patch)
}

// 添加容器的patch
// 如果是第一个patch 需要在path末尾添加 /-
func addContainer(target, added []corev1.Container, basePath string) (patch []patchOperation) {
	first := len(target) == 0
	var value interface{}
	for _, add := range added {
		value = add
		path := basePath
		if first {
			first = false
			value = []corev1.Container{add}
		} else {
			path = path + "/-"
		}
		glog.Infof("[addContainer.value][add:+v][value:+v] ", add, value)
		patch = append(patch, patchOperation{
			Op:    "add",
			Path:  path,
			Value: value,
		})
	}

	return patch
}

func addVolume(target, added []corev1.Volume, basePath string) (patch []patchOperation) {
	first := len(target) == 0
	var value interface{}
	for _, add := range added {
		value = add
		path := basePath
		if first {
			first = false
			value = []corev1.Volume{add}
		} else {
			path = path + "/-"
		}
		patch = append(patch, patchOperation{
			Op:    "add",
			Path:  path,
			Value: value,
		})
	}
	return patch
}

func updateAnnotation(target map[string]string, added map[string]string) (patch []patchOperation) {
	for key, value := range added {
		if target == nil || target[key] == "" {
			target = map[string]string{}
			patch = append(patch, patchOperation{
				Op:   "add",
				Path: "/metadata/annotations",
				Value: map[string]string{
					key: value,
				},
			})
		} else {
			patch = append(patch, patchOperation{
				Op:    "replace",
				Path:  "/metadata/annotations/" + key,
				Value: value,
			})
		}
	}
	return patch
}

func loadConfig(configFile string) (*Config, error) {
	data, err := ioutil.ReadFile(configFile)
	if err != nil {
		return nil, err
	}
	glog.Infof("New configuration: from file:%s ", configFile)

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}
