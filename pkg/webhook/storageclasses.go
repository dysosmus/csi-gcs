package webhook

import "sync"

// driverStorageClassesSet concurrent safe set to cache StorageClass backed by the driver.
type driverStorageClassesSet struct {
	m sync.RWMutex
	classes map[string]struct{}
}

func (s *driverStorageClassesSet) has(className string) bool {
	s.m.RLock()
	defer s.m.RUnlock()
	_, exist := s.classes[className]
	return exist
}

func (s *driverStorageClassesSet) add(className string)  {
	s.m.Lock()
	defer s.m.Unlock()
	s.classes[className] = struct{}{}
}

func (s *driverStorageClassesSet) remove(className string)  {
	s.m.Lock()
	defer s.m.Unlock()
	delete(s.classes, className)
}

