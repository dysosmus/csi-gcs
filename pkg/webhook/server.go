package webhook

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/ofek/csi-gcs/pkg/util"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog"
)

type handler struct {
	k8sClient                   *kubernetes.Clientset
	driverReadyLabel            string
	driverReadySelectorPodPatch []byte
	driverName                  string
	driverStorageClasses        driverStorageClassesSet
}

func NewServer(driverName string) (http.Handler, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	patch := []struct {
		Op    string `json:"op"`
		Path  string `json:"path"`
		Value string `json:"value"`
	}{{
		Op:    "add",
		Path:  "/spec/nodeSelector/" + util.DriverReadyLabelJSONPatchEscaped(driverName),
		Value: "true",
	}}
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return nil, err
	}

	h := handler{
		k8sClient:                   clientset,
		driverReadyLabel:            util.DriverReadyLabel(driverName),
		driverReadySelectorPodPatch: patchBytes,
		driverName:                  driverName,
		driverStorageClasses: driverStorageClassesSet{
			classes: make(map[string]struct{}),
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/mutate-inject-driver-ready-selector", h.handleInjectDriverReadySelector)
	mux.HandleFunc("/healthz", h.handleHealthz)

	lw := cache.NewListWatchFromClient(
		clientset.StorageV1().RESTClient(),
		"storageclasses",
		metav1.NamespaceAll,
		fields.Everything(),
	)
	_, c := cache.NewInformer(lw,  &storagev1.StorageClass{}, 10 * time.Second, cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			stc, ok := obj.(*storagev1.StorageClass)
			if !ok {
				klog.Warningf("Received unexpected create event '%v'", obj)
				return
			}
			if stc.Provisioner == driverName {
				klog.V(6).Infof("Adding '%s' from known storage class", stc.Name)
				h.driverStorageClasses.add(stc.Name)
				if stc.Annotations["storageclass.kubernetes.io/is-default-class"] == "true" {
					h.driverStorageClasses.add("")
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
			stc, ok := obj.(*storagev1.StorageClass)
			if !ok {
				klog.Warningf("Received unexpected delete event '%v'", obj)
				return
			}
			if stc.Provisioner == driverName {
				klog.V(6).Infof("Removing '%s' from known storage class", stc.Name)
				h.driverStorageClasses.remove(stc.Name)
				if stc.Annotations["storageclass.kubernetes.io/is-default-class"] == "true" {
					h.driverStorageClasses.remove("")
				}
			}
		},
		UpdateFunc: func(oldo, newo interface{}) {
			newstc, ok := newo.(*storagev1.StorageClass)
			if !ok {
				klog.Warningf("Received unexpected update event '%v'", newo)
				return
			}

			if newstc.Provisioner == driverName {
				klog.V(6).Infof("Adding '%s' to known storage class", newstc.Name)
				h.driverStorageClasses.add(newstc.Name)
				if newstc.Annotations["storageclass.kubernetes.io/is-default-class"] == "true" {
					h.driverStorageClasses.add("")
				}
				return
			}
		},
	})
	var stopCh <- chan struct{}
	go c.Run(stopCh)

	return mux, nil
}

func (h *handler) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
	return
}

func (h *handler) handleInjectDriverReadySelector(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}

	admrev := admissionv1.AdmissionReview{}
	err := json.NewDecoder(r.Body).Decode(&admrev)
	if err != nil {
		http.Error(w, "unable to decode request: "+err.Error(), http.StatusBadRequest)
		return
	}

	if admrev.Request.Operation != admissionv1.Create {
		http.Error(w, "unsupported admission operation, operation must be 'create'", http.StatusBadRequest)
		return
	}

	pod := corev1.Pod{}
	err = json.Unmarshal(admrev.Request.Object.Raw, &pod)
	klog.V(6).Infof("Received '%s'", string(admrev.Request.Object.Raw))
	if err != nil {
		http.Error(w, "unable to decode request object, expected v1/Pod: "+err.Error(), http.StatusBadRequest)
		return
	}
	// The namespace need to be populated from the AdmissionReview Request info
	// as the pod doesn't necessarily has a namespace yet.
	pod.Namespace = admrev.Request.Namespace

	admresp := admissionv1.AdmissionResponse{
		UID:     admrev.Request.UID,
		Allowed: true,
	}
	if podHasDriverReadyLabelSelectorOrAffinity(&pod, h.driverReadyLabel) {
		klog.V(5).Infof("Skipping pod %s/%s already has driver ready preference", pod.Namespace, pod.Name)
	} else {
		if podHasCsiGCSVolume(&pod, h.driverName, h.k8sClient.CoreV1(), h.driverStorageClasses) {
			klog.V(5).Infof("Mutating pod %s/%s", pod.Namespace, pod.Name)
			patchType := admissionv1.PatchTypeJSONPatch
			admresp.PatchType = &patchType
			admresp.Patch = h.driverReadySelectorPodPatch
		} else {
			klog.V(5).Infof("Skipping pod %s/%s doesn't has csi-gcs volume", pod.Namespace, pod.Name)
		}
	}

	jsonOKResponse(w, &admissionv1.AdmissionReview{
		TypeMeta: admrev.TypeMeta,
		Response: &admresp,
	})
	return
}

func jsonOKResponse(w http.ResponseWriter, rsp interface{}) {
	bts, err := json.Marshal(rsp)
	if err != nil {
		http.Error(w, "unable to encode response: "+err.Error(), http.StatusInternalServerError)
		return
	}
	klog.V(6).Infof("Answering '%s'", string(bts))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(bts)
}
