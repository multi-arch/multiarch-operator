package core

import (
	"context"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/klog/v2"
	"reflect"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"time"
)

// NewSingleObjectEventHandler creates a new event handler for a single object.
// It exploits a WithWatch client to watch for changes on the object and execute a goroutine to listen for the channel
// and call the handler function when an event occurs.
// The function is generic and takes two types T and L, where T is the type of the object to watch and L is the type of
// the list of T objects to watch. The function also takes the name of the object the handler should subscribe to
// and the namespace to watch (use an empty string for the namespace if the resource is cluster-scoped).
// handler is a function that takes the event type and the object that was changed. Event types are defined in watch.go
// and can be Added, Modified, Deleted, Bookmark and Error (not handled by handler).
// errorHandler is an optional (nullable pointer to a) function executed when the event type is Error.
func NewSingleObjectEventHandler[T client.Object, L client.ObjectList](ctx context.Context,
	name string, namespace string, pollingInterval time.Duration,
	handler func(watch.EventType, T), errorHandler *func(*metav1.Status)) error {

	cfg := config.GetConfigOrDie()

	cli, err := client.NewWithWatch(cfg, client.Options{})
	if err != nil {
		return err
	}
	list := reflect.New(reflect.TypeOf((*L)(nil)).Elem().Elem()).Interface().(L)
	lop := &client.ListOptions{}
	if namespace != "" {
		lop.Namespace = namespace
	}
	w, err := cli.Watch(ctx, list, lop)
	if err != nil {
		return err
	}

	go func() {
		for {
			select {
			case e := <-w.ResultChan():
				switch eventType := e.Type; eventType {
				case watch.Added, watch.Modified, watch.Deleted, watch.Bookmark:
					if e.Object != nil && e.Object.(metav1.Object).GetName() != name {
						continue
					}
					handler(eventType, e.Object.DeepCopyObject().(T))
				case watch.Error:
					if e.Object != nil && errorHandler != nil {
						obj := e.Object.(*metav1.Status)
						(*errorHandler)(obj)
					}
				default:
					klog.Warningf("Event type not handled: %+v", eventType)
				}
			}
		}
	}()

	obj := reflect.New(reflect.TypeOf((*T)(nil)).Elem().Elem()).Interface().(T)
	getAndHandle := func() error {
		err := cli.Get(ctx, client.ObjectKey{
			Namespace: namespace,
			Name:      name,
		}, obj)
		if err != nil {
			klog.Errorf("Error getting object %s/%s: %v", namespace, name, err)
			return err
		}
		handler(watch.Modified, obj)
		return nil
	}
	// getAndHandle is called at the end of this function to force a first synchronous get and make the
	// lazy initialization working correctly.
	// If we don't force the initial get, we can incur in a race condition for which the goroutine has not get and stored
	// the globalPullSecret yet and the remote inspection tries to get it from the cache (returning nil).
	if pollingInterval == 0 {
		return getAndHandle()
	}
	// Use polling to periodically get the obj and execute the handler to guarantee robustness against the loss of watch events.
	ticker := time.NewTicker(pollingInterval * time.Second)
	go func() {
		for {
			select {
			case <-ticker.C:
				_ = getAndHandle()
			}
		}
	}()
	return getAndHandle()
}
