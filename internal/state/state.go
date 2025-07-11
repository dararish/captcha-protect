package state

import (
	"reflect"

	lru "github.com/patrickmn/go-cache"
)

type StateItem struct {
	Value      interface{} `json:"value"`
	Expiration int64       `json:"expiration"` // Unix timestamp, 0 means no expiration
}

type State struct {
	Rate     map[string]StateItem `json:"rate"`
	Bots     map[string]StateItem `json:"bots"`
	Verified map[string]StateItem `json:"verified"`
	Memory   map[string]uintptr   `json:"memory"`
}

func GetState(rateCache, botCache, verifiedCache map[string]lru.Item) State {
	state := State{
		Memory: make(map[string]uintptr, 3),
	}

	state.Rate = make(map[string]StateItem, len(rateCache))
	state.Memory["rate"] = reflect.TypeOf(state.Rate).Size()
	for k, v := range rateCache {
		expiration := int64(0)
		if v.Expiration > 0 {
			expiration = v.Expiration
		}
		state.Rate[k] = StateItem{
			Value:      v.Object.(uint),
			Expiration: expiration,
		}
		state.Memory["rate"] += reflect.TypeOf(k).Size()
		state.Memory["rate"] += reflect.TypeOf(v).Size()
		state.Memory["rate"] += uintptr(len(k))
	}

	state.Bots = make(map[string]StateItem, len(botCache))
	state.Memory["bot"] = reflect.TypeOf(state.Bots).Size()
	for k, v := range botCache {
		expiration := int64(0)
		if v.Expiration > 0 {
			expiration = v.Expiration
		}
		state.Bots[k] = StateItem{
			Value:      v.Object.(bool),
			Expiration: expiration,
		}
		state.Memory["bot"] += reflect.TypeOf(k).Size()
		state.Memory["bot"] += reflect.TypeOf(v).Size()
		state.Memory["bot"] += uintptr(len(k))
	}

	state.Verified = make(map[string]StateItem, len(verifiedCache))
	state.Memory["verified"] = reflect.TypeOf(state.Verified).Size()
	for k, v := range verifiedCache {
		expiration := int64(0)
		if v.Expiration > 0 {
			expiration = v.Expiration
		}
		state.Verified[k] = StateItem{
			Value:      v.Object.(bool),
			Expiration: expiration,
		}
		state.Memory["verified"] += reflect.TypeOf(k).Size()
		state.Memory["verified"] += reflect.TypeOf(v).Size()
		state.Memory["verified"] += uintptr(len(k))
	}

	return state
}
