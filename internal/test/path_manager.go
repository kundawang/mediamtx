package test

import (
	"github.com/bluenviron/mediamtx/internal/defs"
)

// PathManager is a dummy path manager.
type PathManager struct {
	FindPathConfImpl func(req defs.PathFindPathConfReq) (*defs.PathFindPathConfRes, error)
	DescribeImpl     func(req defs.PathDescribeReq) (*defs.PathDescribeRes, error)
	AddPublisherImpl func(req defs.PathAddPublisherReq) (*defs.PathAddPublisherRes, error)
	AddReaderImpl    func(req defs.PathAddReaderReq) (*defs.PathAddReaderRes, error)
}

// FindPathConf implements PathManager.
func (pm *PathManager) FindPathConf(req defs.PathFindPathConfReq) (*defs.PathFindPathConfRes, error) {
	if pm.FindPathConfImpl == nil {
		return nil, defs.ErrPathRedirectUnsupported
	}
	return pm.FindPathConfImpl(req)
}

// Describe implements PathManager.
func (pm *PathManager) Describe(req defs.PathDescribeReq) (*defs.PathDescribeRes, error) {
	if pm.DescribeImpl == nil {
		return nil, defs.ErrPathRedirectUnsupported
	}
	return pm.DescribeImpl(req)
}

// AddPublisher implements PathManager.
func (pm *PathManager) AddPublisher(req defs.PathAddPublisherReq) (*defs.PathAddPublisherRes, error) {
	if pm.AddPublisherImpl == nil {
		return nil, defs.ErrPathRedirectUnsupported
	}
	return pm.AddPublisherImpl(req)
}

// AddReader implements PathManager.
func (pm *PathManager) AddReader(req defs.PathAddReaderReq) (*defs.PathAddReaderRes, error) {
	if pm.AddReaderImpl == nil {
		return nil, &defs.PathNoStreamAvailableError{PathName: req.AccessRequest.Name}
	}
	return pm.AddReaderImpl(req)
}
