package gui

import (
	"github.com/skycoin/cxo/schema"
	"fmt"
)

type schemaApi struct {
	sm *schema.Container
}

//RegisterNodeManagerHandlers - create routes for NodeManager
func RegisterSchemaHandlers(router *Router, schemaProvider *schema.Container) {
	// enclose shm into SkyhashManager to be able to add methods
	//lshm := SkyhashManager{Manager: shm}

	sch := &schemaApi{sm:schemaProvider}

	router.GET("/object1/:object/list", sch.List)
	router.GET("/object1/:object/schema", sch.Schema)
}

func (api *schemaApi) List(ctx *Context) error {
	objectName := *ctx.Param("object")
	schemaKey, err := api.sm.GetSchemaKey(objectName)
	if (err != nil) {
		return ctx.ErrNotFound(err.Error(), "schema", objectName)
	}

	keys := api.sm.GetAllBySchema(schemaKey)
	schema, err := api.sm.GetSchema(objectName)

	res := []map[string]string{}

	for i := 0; i < len(keys); i++ {
		res = append(res, api.sm.LoadFields(keys[i], schema))
	}

	return ctx.JSON(200, res)
}

func (api *schemaApi) Schema(ctx *Context) error {
	objectName := *ctx.Param("object")
	schema, err := api.sm.GetSchema(objectName)
	fmt.Println("Schema", schema)
	if (err != nil) {
		return ctx.ErrNotFound(err.Error(), "schema", objectName)
	}
	return ctx.JSON(200, schema)
}
