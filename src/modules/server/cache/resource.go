package cache

import (
	"sync"

	"github.com/didi/nightingale/v4/src/models"
)

var ResourceCache *ResourceMap

type ResourceMap struct {
	sync.RWMutex
	Data map[int64]*models.Resource
}

func NewResourceCache() *ResourceMap {
	return &ResourceMap{Data: make(map[int64]*models.Resource)}
}

func (t *ResourceMap) GetBy(id int64) *models.Resource {
	t.RLock()
	defer t.RUnlock()

	return t.Data[id]
}

func (t *ResourceMap) GetByIds(ids []int64) []*models.Resource {
	t.RLock()
	defer t.RUnlock()
	var objs []*models.Resource
	for _, id := range ids {
		if o, ok := t.Data[id]; ok {
			objs = append(objs, o)
		}
	}

	return objs
}

func (t *ResourceMap) SetAll(objs map[int64]*models.Resource) {
	t.Lock()
	defer t.Unlock()

	t.Data = objs
	return
}
