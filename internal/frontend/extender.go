package frontend

import (
	"context"
	"errors"
	"time"

	"github.com/murasame29/workload-knocker/internal/config"
	"github.com/murasame29/workload-knocker/internal/frontend/notify"
)

// serviceExtender adapts Service.Extend to notify.Extender; authorization, mode and the cap stay in the service.
type serviceExtender struct {
	svc      *Service
	clusters config.Resolver
}

// NewExtender returns the Extend button handler backed by svc.
func NewExtender(svc *Service, clusters config.Resolver) notify.Extender {
	return serviceExtender{svc: svc, clusters: clusters}
}

func (e serviceExtender) Extend(_ context.Context, req notify.ExtendRequest) (time.Time, error) {
	cl, ok := e.clusters.ByEnv(req.Ref.Env)
	if !ok {
		return time.Time{}, errors.New("the cluster is no longer in the registry")
	}
	res := e.svc.Extend(ExtendRequest{Cluster: cl, Namespace: req.Ref.Namespace, Subject: req.Subject, Names: []string{req.Ref.Name}, Duration: req.By})
	if len(res.Rejected) > 0 {
		return time.Time{}, errors.New(res.Rejected[0].Message())
	}
	if len(res.Extended) == 0 {
		return time.Time{}, errors.New("nothing was extended")
	}
	if x := res.Extended[0]; x.Err != nil {
		return time.Time{}, x.Err
	}
	return res.Extended[0].StopAt, nil
}
