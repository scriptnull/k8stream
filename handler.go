package main

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientscheme "k8s.io/client-go/kubernetes/scheme"
)

const (
	eventCacheTable = "events"
)

type Handler struct {
	client *kubernetesClient
	ch     chan<- interface{}
	db     Cachier
}

func (h *Handler) OnAdd(obj interface{}) {
	var err error
	switch obj.(type) {
	case *v1.Event:
		event := obj.(*v1.Event)
		err = h.onEvent(event)
	case *v1.Service:
		err = h.onService(obj.(*v1.Service), "addedService")
	}

	if err != nil {
		log.Println(err)
	}
}

func (h *Handler) OnUpdate(oldObj, newObj interface{}) {
	var err error
	switch newObj.(type) {
	case *v1.Event:
		event := newObj.(*v1.Event)
		err = h.onEvent(event)
	case *v1.Service:
		err = h.onService(newObj.(*v1.Service), "updatedService")
	}

	if err != nil {
		log.Println(err)
	}
}

func (h *Handler) OnDelete(obj interface{}) {
	var err error
	switch obj.(type) {
	case *v1.Event:
		event := obj.(*v1.Event)
		err = h.onEvent(event)
	case *v1.Service:
		err = h.onService(obj.(*v1.Service), "deletedService")
	}

	if err != nil {
		log.Println(err)
	}
}

func (h *Handler) onService(s *v1.Service, eventType string) error {
	// Do not watch the default kubernetes services
	switch s.GetNamespace() {
	case "kube-system", "kubernetes-dashboard":
		return nil
	default:
		if s.GetName() == "kubernetes" {
			return nil
		}
	}

	suid := string(s.GetUID())

	r, err := h.db.Get(eventCacheTable, suid)
	if err != nil {
		return err
	}

	// Service has been processed already.
	if r.Exists() {
		var existingService L9Event
		if err := r.Unmarshal(&existingService); err != nil {
			return err
		}

		// should process update events for a service too, but ignore if event is already processed.
		if existingService.ReferenceVersion >= s.GetResourceVersion() {
			log.Println("Service", suid, "already processed")
			return nil
		}
	}

	// Save service to database
	// Maybe use s.SelfLink since UID is literally not exposed elsewhere for
	// a service other than the service itself being aware of it.
	// And a change in the service will change the UID afterall.
	if err := h.db.Set("service", suid, s); err != nil {
		return err
	}

	// Find all PODS for this service so that a rerverse lookup is possible.
	pods, err := h.client.getPods(h.db, s)
	if err != nil {
		return err
	}

	// Save service -> pods
	if err := h.db.Set("service-pods", suid, pods); err != nil {
		return err
	}

	// Also save pod -> service denormalized for reverse Index lookup
	for _, p := range pods {
		// A pod may be behind multiple services.
		// Get the existing array. append the new serviceID and set again
		// Calls for race condition probably. So will need some mutex here.
		if err := h.db.Set(
			makeKey("pod-service", string(p.GetUID())), suid, true,
		); err != nil {
			return err
		}
	}

	h.ch <- makeL9ServiceEvent(h.db, s, pods, eventType)
	return nil
}

func (h *Handler) onEvent(e *v1.Event) error {
	// Do not watch the default kubernetes services
	switch e.GetNamespace() {
	case "kube-system", "kubernetes", "kubernetes-dashboard":
		return nil
	}

	r, err := h.db.Get(eventCacheTable, string(e.UID))
	if err != nil {
		return err
	}

	// Event has been processed already.
	if r.Exists() {
		return nil
	}

	obj, err := h.client.getObject(h.db, &e.InvolvedObject)
	if err != nil {
		log.Println(err)
	}

	addr, err := h.client.getNodeAddress(h.db, e.Source.Host)
	if err != nil {
		log.Println(err)
	}

	h.ch <- makeL9Event(h.db, e, obj, addr)
	return nil
}

type L9Event struct {
	ID                 string                 `json:"id,omitempty"`
	Timestamp          int64                  `json:"timestamp,omitempty"`
	Component          string                 `json:"component,omitempty"`
	Host               string                 `json:"host,omitempty"`
	Message            string                 `json:"message,omitempty"`
	Namespace          string                 `json:"namespace,omitempty"`
	Reason             string                 `json:"reason,omitempty"`
	ReferenceUID       string                 `json:"reference_uid,omitempty"`
	ReferenceNamespace string                 `json:"reference_namespace,omitempty"`
	ReferenceName      string                 `json:"reference_name,omitempty"`
	ReferenceKind      string                 `json:"reference_kind,omitempty"`
	ReferenceVersion   string                 `json:"reference_version,omitempty"`
	ObjectUid          string                 `json:"object_uid,omitempty"`
	Labels             map[string]string      `json:"labels,omitempty"`
	Annotations        map[string]string      `json:"annotations,omitempty"`
	Address            []string               `json:"address,omitempty"`
	Pod                map[string]interface{} `json:"pod,omitempty"`
}

func makeL9ServiceEvent(db Cachier, s *v1.Service, pods []v1.Pod, eventType string) *L9Event {
	podMap := map[string]interface{}{}
	for _, p := range pods {
		b, err := json.Marshal(miniPodInfo(p))
		if err != nil {
			podMap[p.GetName()] = err.Error()
		} else {
			podMap[p.GetName()] = string(b)
		}
	}

	return &L9Event{
		ID:                 fmt.Sprintf("%s-%s", s.GetUID(), s.GetResourceVersion()),
		Timestamp:          time.Now().Unix(),
		Component:          s.GetName(),
		Host:               "",
		Message:            eventType,
		Namespace:          s.GetNamespace(),
		Reason:             eventType,
		ReferenceUID:       "",
		ReferenceNamespace: "",
		ReferenceName:      "",
		ReferenceKind:      "",
		ReferenceVersion:   s.GetResourceVersion(),
		ObjectUid:          string(s.GetUID()),
		Labels:             s.GetLabels(),
		Annotations:        s.GetAnnotations(),
		Address:            nil,
		Pod:                podMap,
	}
}

func makeL9Event(
	db Cachier, e *v1.Event, u *unstructured.Unstructured, address []string,
) *L9Event {
	ne := &L9Event{
		ID:               string(e.UID),
		Timestamp:        e.CreationTimestamp.Time.Unix(),
		Component:        e.Source.Component,
		Host:             e.Source.Host,
		Message:          e.Message,
		Namespace:        e.Namespace,
		Reason:           e.Reason,
		ReferenceUID:     string(e.InvolvedObject.UID),
		ReferenceName:    e.InvolvedObject.Name,
		ReferenceVersion: e.InvolvedObject.APIVersion,
		Address:          address,
	}

	if u != nil {
		if err := addPodDetails(db, ne, u); err != nil {
			log.Println(err)
		}

		// ne.InvolvedObject = u
		ne.ReferenceNamespace = u.GetNamespace()
		ne.ReferenceKind = u.GetKind()
		ne.ObjectUid = string(u.GetUID())
		ne.Labels = u.GetLabels()
		ne.Annotations = u.GetAnnotations()
	}

	return ne
}

func addPodDetails(db Cachier, ne *L9Event, u *unstructured.Unstructured) error {
	if strings.ToLower(u.GetKind()) != "pod" {
		return nil
	}

	p, err := unstructuredToPod(u)
	if err != nil {
		return err
	}

	ne.Pod = miniPodInfo(*p)
	ne.Pod["impacted_services"], err = getPodServices(db, string(p.GetUID()))
	return err
}

func miniPodInfo(p v1.Pod) map[string]interface{} {
	ne := map[string]interface{}{}
	ne["uid"] = p.GetUID()
	ne["name"] = p.GetName()
	ne["namespace"] = p.GetNamespace()
	ne["start_time"] = p.Status.StartTime
	ne["ip"] = p.Status.PodIP
	ne["host_ip"] = p.Status.HostIP
	return ne
}

func unstructuredToPod(obj *unstructured.Unstructured) (*v1.Pod, error) {
	json, err := runtime.Encode(unstructured.UnstructuredJSONScheme, obj)
	if err != nil {
		return nil, err
	}

	pod := new(v1.Pod)
	err = runtime.DecodeInto(clientscheme.Codecs.LegacyCodec(v1.SchemeGroupVersion), json, pod)
	pod.Kind = ""
	pod.APIVersion = ""
	return pod, err
}

func getPodServices(db Cachier, uid string) ([]string, error) {
	// DB currently does not have a list method.
	// We have treated each pod as a seaprate Index, so a prefix should help
	// hunting all keys that were set with the prefix of pod-service-podId
	// Need to expose a method in DB.
	serviceIds, err := db.List(makeKey("pod-service", uid))
	if err != nil {
		return nil, err
	}
	services := []string{}
	for _, sId := range serviceIds {
		res, err := db.Get("service", sId)
		if err == nil && res.Exists() {
			var v *v1.Service
			if err := res.Unmarshal(&v); err != nil {
				log.Println(err)
				continue
			}
			services = append(services, v.GetName())
		}
	}
	return services, err
}
