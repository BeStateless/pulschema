// Copyright 2022, Cloudy Sky Software.

package pkg

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/golang/glog"

	"github.com/pkg/errors"

	"github.com/pulumi/pulumi/pkg/v3/codegen"
	dotnetgen "github.com/pulumi/pulumi/pkg/v3/codegen/dotnet"
	pschema "github.com/pulumi/pulumi/pkg/v3/codegen/schema"

	"github.com/pulumi/pulumi/sdk/v3/go/common/util/contract"
)

const (
	componentsSchemaRefPrefix = "#/components/schemas/"
	jsonMimeType              = "application/json"
	arrayType                 = "array"
	parameterLocationPath     = "path"
	pathSeparator             = "/"
)

var versionRegex = regexp.MustCompile("v[0-9]+[a-z0-9]*")

// OpenAPIContext represents an OpenAPI spec from which a Pulumi package
// spec can be extracted.
type OpenAPIContext struct {
	// Doc is the parsed, validated OpenAPI spec.
	Doc openapi3.T
	// Pkg is the Pulumi schema spec.
	Pkg *pschema.PackageSpec
	// ExcludedPaths is a slice of API endpoint paths
	// that should be skipped.
	ExcludedPaths []string
	// UseParentResourceAsModule indicates whether an endpoint
	// operation's parent resource should be used as the module
	// for a resource rather than using the root path of the
	// endpoint.
	// For example, when extracting a resource for the endpoint
	// `/rootResource/v1/subResource`, with this set to `true`,
	// the `subResource` will be under the module `subResource`
	// instead of `rootResource` module. This is useful to avoid
	// conflicts arising from properties named similarly in different
	// resource that are actually different despite their names.
	//
	// Another example is `rootResource/v1/subResource/{id}/secondResource`.
	// The resource called `secondResource` will be in a module called
	// `subResource` instead of a module called `rootResource`.
	UseParentResourceAsModule bool

	// OperationIdsHaveTypeSpecNamespace indicates if the API operation IDs
	// are separated by the CADL namespace they were defined in.
	OperationIdsHaveTypeSpecNamespace bool

	// TypeSpecNamespaceSeparator is the separator used in the operationId value.
	TypeSpecNamespaceSeparator string

	// resourceCRUDMap is a map of the Pulumi resource type
	// token to its CRUD endpoints.
	resourceCRUDMap map[string]*CRUDOperationsMap
	// autoNameMap is a map of the resource type token
	// and the property that can be auto-named.
	autoNameMap map[string]string
}

type duplicateEnumError struct {
	msg string
}

func (d *duplicateEnumError) Error() string {
	return d.msg
}

func getModuleFromPath(path string, useParentResourceAsModule bool) string {
	if useParentResourceAsModule {
		parentPath := getParentPath(path)
		parentParts := strings.Split(strings.TrimPrefix(parentPath, pathSeparator), pathSeparator)
		return parentParts[len(parentParts)-1]
	}

	parts := strings.Split(strings.TrimPrefix(path, pathSeparator), pathSeparator)

	// If the first item in parts is not a version number prefix, then
	// return as-is.
	if !versionRegex.Match([]byte(parts[0])) {
		return parts[0]
	}

	// Otherwise, we should use a versioned module.
	return parts[1] + pathSeparator + parts[0]
}

func getParentPath(path string) string {
	parts := strings.Split(strings.TrimPrefix(path, pathSeparator), pathSeparator)
	lastPathPart := parts[len(parts)-1]
	if !strings.HasPrefix(lastPathPart, "{") && !strings.HasSuffix(lastPathPart, "}") {
		return path
	}

	// Skip the last path part which contains a path parameter.
	return pathSeparator + strings.Join(parts[0:len(parts)-1], pathSeparator)
}

// index returns the index of the element `toFind`
// in the slice `slice`. Returns -1 if not found.
func index(slice []string, toFind string) int {
	for i, s := range slice {
		if s == toFind {
			return i
		}
	}

	return -1
}

func getResourceTitleFromOperationID(originalOperationID, method string, isSeparatedByTypeSpecNamespace bool) string {
	var replaceKeywords []string

	switch method {
	case http.MethodDelete:
		replaceKeywords = append(replaceKeywords, "delete")
	case http.MethodGet:
		replaceKeywords = append(replaceKeywords, "get", "list")
	case http.MethodPatch:
		replaceKeywords = append(replaceKeywords, "update")
	case http.MethodPost:
		replaceKeywords = append(replaceKeywords, "create", "set", "post", "put")
	case http.MethodPut:
		replaceKeywords = append(replaceKeywords, "update", "create", "set", "put")
	}

	result := originalOperationID

	// TypeSpec-generated operations can have an operation ID separated by the namespace
	// the operation is defined in.
	if isSeparatedByTypeSpecNamespace {
		parts := strings.Split(originalOperationID, "_")
		result = parts[len(parts)-1]
	} else if strings.Contains(originalOperationID, "_") {
		parts := strings.Split(originalOperationID, "_")
		result = parts[0]
		for _, p := range parts[1:] {
			result += ToPascalCase(p)
		}
	}

	for _, v := range replaceKeywords {
		result = strings.ReplaceAll(result, v, "")
		result = strings.ReplaceAll(result, ToPascalCase(v), "")
	}

	resourceTitle := ToPascalCase(result)

	glog.Infof("converted operation ID %s to resource title %s\n", originalOperationID, resourceTitle)

	return resourceTitle
}

func ensureRequestSchemaTitle(schemaName string, schemaRef *openapi3.SchemaRef) {
	if schemaRef.Value.Title != "" {
		return
	}

	if strings.Contains(schemaName, "_") {
		schemaRef.Value.Title = ToPascalCase(schemaName)
	} else {
		parts := strings.Split(schemaName, "_")
		result := parts[0]
		for _, p := range parts[1:] {
			result += ToPascalCase(p)
		}

		schemaRef.Value.Title = result
	}
}

// ensureIDHierarchyInRequestPath ensures that the IDs in the path
// segment following the rules:
//
// 1. parent resource id param should not be called `id`.
//
// 2. the sub-resource, if present, should use `id` if the path
// variable represents an ID.
func ensureIDHierarchyInRequestPath(path string, pathItem *openapi3.PathItem) string {
	segments := strings.Split(path, pathSeparator)
	numSegments := len(segments)

	pathParamTransformationsMap := make(map[string]string)

	updatePathParamNames := func(params openapi3.Parameters) {
		for _, param := range params {
			if param.Value.In != "path" {
				continue
			}

			if transformedName, ok := pathParamTransformationsMap["{"+param.Value.Name+"}"]; ok {
				param.Value.Name = transformedName
			}
		}
	}

	for i, segment := range segments {
		if segment == "" || !strings.Contains(segment, "{") {
			continue
		}

		var transformedParam string

		// Satisfies rule #1.
		if i != numSegments-1 && segment == "id" {
			parentResource := segments[i-1]
			transformedParam = parentResource
			if strings.Contains(parentResource, "_") {
				transformedParam += "_id"
			} else {
				transformedParam += "Id"
			}
		} else if i == numSegments-1 && segment != "id" && strings.Contains(strings.ToLower(segment), "id") {
			// Satisfies rule #2.
			transformedParam = "id"
		}

		if transformedParam == "" {
			continue
		}

		pathParamTransformationsMap[segments[i]] = transformedParam
		segments[i] = "{" + transformedParam + "}"
	}

	if pathItem.Delete != nil && len(pathItem.Delete.Parameters) > 0 {
		updatePathParamNames(pathItem.Delete.Parameters)
	}
	if pathItem.Get != nil && len(pathItem.Get.Parameters) > 0 {
		updatePathParamNames(pathItem.Get.Parameters)
	}
	if pathItem.Patch != nil && len(pathItem.Patch.Parameters) > 0 {
		updatePathParamNames(pathItem.Patch.Parameters)
	}
	if pathItem.Put != nil && len(pathItem.Put.Parameters) > 0 {
		updatePathParamNames(pathItem.Put.Parameters)
	}
	if pathItem.Post != nil && len(pathItem.Post.Parameters) > 0 {
		updatePathParamNames(pathItem.Post.Parameters)
	}

	return strings.Join(segments, pathSeparator)
}

// GatherResourcesFromAPI gathers resources from API endpoints.
// The goal is to extract resources and map their corresponding CRUD
// operations.
//
//   - The "create" operation (denoted by a Post request) determines the schema
//     for the resource.
//   - The "read" operation (denoted by a Get request) determines the schema
//     for "invokes" or "resource get's".
//   - The "update" operation (denoted by a Patch request) determines the schema
//     for resource updates. The Patch request schema is used to determine
//     which properties can be patched when changes are detected in Diff() vs.
//     which ones will force a resource replacement.
func (o *OpenAPIContext) GatherResourcesFromAPI(csharpNamespaces map[string]string) (*ProviderMetadata, openapi3.T, error) {
	o.resourceCRUDMap = make(map[string]*CRUDOperationsMap)
	o.autoNameMap = make(map[string]string)

	for path, pathItem := range o.Doc.Paths {
		// Capture the iteration variable `path` because we use its pointer
		// in the crudMap.
		currentPath := ensureIDHierarchyInRequestPath(path, pathItem)
		module := getModuleFromPath(currentPath, o.UseParentResourceAsModule)

		if index(o.ExcludedPaths, path) > -1 {
			continue
		}

		glog.V(3).Infof("Processing path %s as %s\n", path, currentPath)

		if pathItem.Get != nil {
			parentPath := getParentPath(currentPath)
			glog.V(3).Infof("GET: Parent path for %s is %s\n", currentPath, parentPath)

			jsonReq := pathItem.Get.Responses.Get(200).Value.Content.Get(jsonMimeType)
			if jsonReq.Schema.Value == nil {
				contract.Failf("Path %s has no schema definition for status code 200", currentPath)
			}

			setReadOperationMapping := func(tok string) {
				if existing, ok := o.resourceCRUDMap[tok]; ok {
					existing.R = &currentPath
				} else {
					o.resourceCRUDMap[tok] = &CRUDOperationsMap{
						R: &currentPath,
					}
				}
			}

			resourceType := jsonReq.Schema.Value

			// Use the type and operationID as a hint to determine if this GET endpoint returns a single resource
			// or a list of resources.
			if resourceType.Type != arrayType && !strings.Contains(strings.ToLower(pathItem.Get.OperationID), "list") {
				// If there is a discriminator then we should set this operation
				// as the read endpoint for each of the types in the mapping.
				if resourceType.Discriminator != nil {
					for _, ref := range resourceType.Discriminator.Mapping {
						schemaName := strings.TrimPrefix(ref, componentsSchemaRefPrefix)
						dResource := o.Doc.Components.Schemas[schemaName]
						ensureRequestSchemaTitle(schemaName, dResource)
						typeToken := fmt.Sprintf("%s:%s:%s", o.Pkg.Name, module, dResource.Value.Title)
						setReadOperationMapping(typeToken)

						funcName := "get" + dResource.Value.Title
						funcTypeToken := o.Pkg.Name + ":" + module + ":" + funcName
						getterFuncSpec := o.genGetFunc(*pathItem, *dResource, module, funcName)
						o.Pkg.Functions[funcTypeToken] = getterFuncSpec
						setReadOperationMapping(funcTypeToken)
					}
				} else {
					if resourceType.Title == "" {
						resourceType.Title = getResourceTitleFromOperationID(pathItem.Get.OperationID, http.MethodGet, o.OperationIdsHaveTypeSpecNamespace)
					}

					typeToken := fmt.Sprintf("%s:%s:%s", o.Pkg.Name, module, resourceType.Title)
					setReadOperationMapping(typeToken)

					funcName := "get" + resourceType.Title
					funcTypeToken := o.Pkg.Name + ":" + module + ":" + funcName
					getterFuncSpec := o.genGetFunc(*pathItem, *jsonReq.Schema, module, funcName)
					o.Pkg.Functions[funcTypeToken] = getterFuncSpec
					setReadOperationMapping(funcTypeToken)
				}
			}

			// Add the API operation as a list* function.
			if resourceType.Type == arrayType || strings.Contains(strings.ToLower(pathItem.Get.OperationID), "list") {
				var funcName string
				if resourceType.Title != "" {
					resourceType.Title = strings.ReplaceAll(resourceType.Title, "List", "")
					if !strings.HasPrefix(resourceType.Title, "list") {
						funcName = "list" + resourceType.Title
					}
				} else {
					funcName = "list" + getResourceTitleFromOperationID(pathItem.Get.OperationID, http.MethodGet, o.OperationIdsHaveTypeSpecNamespace)
				}
				funcTypeToken := o.Pkg.Name + ":" + module + ":" + funcName
				funcSpec, err := o.genListFunc(*pathItem, *jsonReq.Schema, funcName, module)
				if err != nil {
					return nil, o.Doc, errors.Wrap(err, "generating list function")
				}

				o.Pkg.Functions[funcTypeToken] = *funcSpec
				setReadOperationMapping(funcTypeToken)
			}
		}

		if pathItem.Patch != nil {
			parentPath := getParentPath(currentPath)
			glog.V(3).Infof("PATCH: Parent path for %s is %s\n", currentPath, parentPath)

			jsonReq := pathItem.Patch.RequestBody.Value.Content.Get(jsonMimeType)
			if jsonReq.Schema.Value == nil {
				contract.Failf("Path %s has no schema definition for Patch method", currentPath)
			}

			setUpdateOperationMapping := func(tok string) {
				if existing, ok := o.resourceCRUDMap[tok]; ok {
					existing.U = &currentPath
				} else {
					o.resourceCRUDMap[tok] = &CRUDOperationsMap{
						U: &currentPath,
					}
				}
			}

			resourceType := jsonReq.Schema.Value
			if resourceType.Title == "" {
				resourceType.Title = getResourceTitleFromOperationID(pathItem.Patch.OperationID, http.MethodPatch, o.OperationIdsHaveTypeSpecNamespace)
			}
			if resourceType.Title == "" {
				return nil, o.Doc, errors.Errorf("patch request body schema must have a title or the operation must have an operationid (path: %s)", currentPath)
			}

			if resourceType.Discriminator != nil || len(resourceType.OneOf) > 0 || len(resourceType.AnyOf) > 0 {
				schemaNames := make([]string, 0)
				if resourceType.Discriminator != nil {
					for _, ref := range resourceType.Discriminator.Mapping {
						schemaName := strings.TrimPrefix(ref, componentsSchemaRefPrefix)
						schemaNames = append(schemaNames, schemaName)
					}
				}

				if len(resourceType.OneOf) > 0 {
					for _, ref := range resourceType.OneOf {
						schemaName := strings.TrimPrefix(ref.Ref, componentsSchemaRefPrefix)
						schemaNames = append(schemaNames, schemaName)
					}
				}

				if len(resourceType.AnyOf) > 0 {
					for _, ref := range resourceType.AnyOf {
						schemaName := strings.TrimPrefix(ref.Ref, componentsSchemaRefPrefix)
						schemaNames = append(schemaNames, schemaName)
					}
				}

				for _, n := range schemaNames {
					dResource := o.Doc.Components.Schemas[n]
					ensureRequestSchemaTitle(n, dResource)
					typeToken := fmt.Sprintf("%s:%s:%s", o.Pkg.Name, module, dResource.Value.Title)
					setUpdateOperationMapping(typeToken)
				}
			} else {
				typeToken := fmt.Sprintf("%s:%s:%s", o.Pkg.Name, module, resourceType.Title)
				setUpdateOperationMapping(typeToken)
			}
		}

		if pathItem.Put != nil {
			parentPath := getParentPath(currentPath)
			glog.V(3).Infof("PUT: Parent path for %s is %s\n", currentPath, parentPath)

			jsonReq := pathItem.Put.RequestBody.Value.Content.Get(jsonMimeType)
			if jsonReq.Schema.Value == nil {
				contract.Failf("Path %s has no schema definition for Put method", currentPath)
			}

			setPutOperationMapping := func(tok string) {
				if existing, ok := o.resourceCRUDMap[tok]; ok {
					existing.P = &currentPath
				} else {
					o.resourceCRUDMap[tok] = &CRUDOperationsMap{
						P: &currentPath,
					}
				}
			}

			resourceType := jsonReq.Schema.Value
			if resourceType.Title == "" {
				resourceType.Title = getResourceTitleFromOperationID(pathItem.Put.OperationID, http.MethodPut, o.OperationIdsHaveTypeSpecNamespace)
			}
			if resourceType.Title == "" {
				return nil, o.Doc, errors.Errorf("put request body schema must have a title or the operation must have an operationid (path: %s)", currentPath)
			}

			if resourceType.Discriminator != nil {
				for _, ref := range resourceType.Discriminator.Mapping {
					schemaName := strings.TrimPrefix(ref, componentsSchemaRefPrefix)
					dResource := o.Doc.Components.Schemas[schemaName]
					ensureRequestSchemaTitle(schemaName, dResource)
					typeToken := fmt.Sprintf("%s:%s:%s", o.Pkg.Name, module, dResource.Value.Title)
					setPutOperationMapping(typeToken)
				}
			} else {
				typeToken := fmt.Sprintf("%s:%s:%s", o.Pkg.Name, module, resourceType.Title)
				setPutOperationMapping(typeToken)
			}

			// PUT methods can be used to create as well as update resources.
			// AS LONG AS the endpoint does not end with a path param. It cannot be used
			// to create resources if the endpoint itself requires the ID of the resource.
			if !strings.HasSuffix(currentPath, "}") {
				if err := o.gatherResource(currentPath, *resourceType, nil /*response type*/, pathItem.Put.Parameters, module); err != nil {
					return nil, o.Doc, errors.Wrapf(err, "generating resource for api path %s", currentPath)
				}

				csharpNamespaces[module] = ToPascalCase(module)
			}
		}

		if pathItem.Delete != nil {
			parentPath := getParentPath(currentPath)
			glog.V(3).Infof("DELETE: Parent path for %s is %s\n", currentPath, parentPath)

			setDeleteOperationMapping := func(tok string) {
				if existing, ok := o.resourceCRUDMap[tok]; ok {
					existing.D = &currentPath
				} else {
					o.resourceCRUDMap[tok] = &CRUDOperationsMap{
						D: &currentPath,
					}
				}
			}

			if pathItem.Delete.RequestBody != nil {
				jsonReq := pathItem.Delete.RequestBody.Value.Content.Get(jsonMimeType)
				if jsonReq.Schema.Value == nil {
					contract.Failf("Path %s has no schema definition for Delete method", currentPath)
				}

				resourceType := jsonReq.Schema.Value
				if resourceType.Title == "" {
					resourceType.Title = getResourceTitleFromOperationID(pathItem.Delete.OperationID, http.MethodDelete, o.OperationIdsHaveTypeSpecNamespace)
				}
				if resourceType.Title == "" {
					return nil, o.Doc, errors.Errorf("delete request body schema must have a title or the operation must have an operationid (path: %s)", currentPath)
				}

				if resourceType.Discriminator != nil {
					for _, ref := range resourceType.Discriminator.Mapping {
						schemaName := strings.TrimPrefix(ref, componentsSchemaRefPrefix)
						dResource := o.Doc.Components.Schemas[schemaName]
						ensureRequestSchemaTitle(schemaName, dResource)
						typeToken := fmt.Sprintf("%s:%s:%s", o.Pkg.Name, module, dResource.Value.Title)
						setDeleteOperationMapping(typeToken)
					}
				} else {
					typeToken := fmt.Sprintf("%s:%s:%s", o.Pkg.Name, module, resourceType.Title)
					setDeleteOperationMapping(typeToken)
				}
			} else {
				resourceTypeTitle := getResourceTitleFromOperationID(pathItem.Delete.OperationID, http.MethodDelete, o.OperationIdsHaveTypeSpecNamespace)
				if resourceTypeTitle == "" {
					return nil, o.Doc, errors.New("request body schema must have a title or the operation must have an operationid")
				}
				typeToken := fmt.Sprintf("%s:%s:%s", o.Pkg.Name, module, resourceTypeTitle)
				setDeleteOperationMapping(typeToken)
			}
		}

		if pathItem.Post == nil {
			continue
		}

		jsonReq := pathItem.Post.RequestBody.Value.Content.Get(jsonMimeType)
		if jsonReq.Schema.Value == nil {
			return nil, o.Doc, errors.Errorf("path %s has no api schema definition for post method", currentPath)
		}

		resourceRequestType := jsonReq.Schema.Value

		// Usually 201 and 202 status codes don't have response bodies,
		// but some OpenAPI specs seem to have a response body for those
		// status codes. For example, DigitalOcean responds with 202
		// for a request to provision Floating IPs that may not be
		// fully provisioned yet.
		responseCodes := []int{200, 201, 202}
		var statusCodeOkResp *openapi3.ResponseRef
		for _, code := range responseCodes {
			statusCodeOkResp = pathItem.Post.Responses.Get(code)

			// Stop looking for response body schema if we found
			// one already.
			if statusCodeOkResp != nil {
				break
			}
		}

		var resourceResponseType *openapi3.Schema
		if statusCodeOkResp != nil {
			jsonResp := statusCodeOkResp.Value.Content.Get(jsonMimeType)
			if jsonResp != nil {
				resourceResponseType = jsonResp.Schema.Value
			}
		}

		if resourceRequestType.Title == "" {
			resourceRequestType.Title = getResourceTitleFromOperationID(pathItem.Post.OperationID, http.MethodPost, o.OperationIdsHaveTypeSpecNamespace)
		}

		if resourceRequestType.Title == "" {
			return nil, o.Doc, errors.Errorf("post request body schema must have a title or the operation must have an operationid (path: %s)", currentPath)
		}

		if err := o.gatherResource(currentPath, *resourceRequestType, resourceResponseType, pathItem.Post.Parameters, module); err != nil {
			return nil, o.Doc, errors.Wrapf(err, "generating resource for api path %s", currentPath)
		}

		csharpNamespaces[module] = ToPascalCase(module)
	}

	return &ProviderMetadata{
		ResourceCRUDMap: o.resourceCRUDMap,
		AutoNameMap:     o.autoNameMap,
	}, o.Doc, nil
}

// genListFunc returns a function spec for a GET API endpoint that returns a list of objects.
// The item type can have a discriminator in the schema. This method will return a type
// that will refer to an output type that uses the discriminator properties to correctly
// type the output result.
func (o *OpenAPIContext) genListFunc(pathItem openapi3.PathItem, returnTypeSchema openapi3.SchemaRef, funcName, module string) (*pschema.FunctionSpec, error) {
	parentName := ToPascalCase(funcName)
	funcPkgCtx := &resourceContext{
		mod:               module,
		pkg:               o.Pkg,
		openapiComponents: *o.Doc.Components,
		visitedTypes:      codegen.NewStringSet(),
	}

	requiredInputs := codegen.NewStringSet()
	inputProps := make(map[string]pschema.PropertySpec)
	for _, param := range pathItem.Get.Parameters {
		if param.Value.In != parameterLocationPath {
			continue
		}

		paramName := param.Value.Name
		inputProps[paramName] = pschema.PropertySpec{
			Description: param.Value.Description,
			TypeSpec:    pschema.TypeSpec{Type: "string"},
		}
		requiredInputs.Add(paramName)
	}

	outputPropType, err := funcPkgCtx.propertyTypeSpec(parentName, returnTypeSchema)
	if err != nil {
		return nil, errors.Wrap(err, "generating property type spec for response schema")
	}

	return &pschema.FunctionSpec{
		Description: pathItem.Description,
		Inputs: &pschema.ObjectTypeSpec{
			Properties: inputProps,
			Required:   requiredInputs.SortedValues(),
		},
		Outputs: &pschema.ObjectTypeSpec{
			Properties: map[string]pschema.PropertySpec{
				"items": {
					TypeSpec: *outputPropType,
				},
			},
			Required: []string{"items"},
		},
	}, nil
}

// genGetFunc returns a function spec for a GET API endpoint that returns a single object.
// The single object can have a discriminator in the schema. This method will return a type
// that will refer to an output type that uses the discriminator properties to correctly
// type the output result.
func (o *OpenAPIContext) genGetFunc(pathItem openapi3.PathItem, returnTypeSchema openapi3.SchemaRef, module, funcName string) pschema.FunctionSpec {
	parentName := ToPascalCase(funcName)
	funcPkgCtx := &resourceContext{
		mod:               module,
		pkg:               o.Pkg,
		openapiComponents: *o.Doc.Components,
		visitedTypes:      codegen.NewStringSet(),
	}

	requiredInputs := codegen.NewStringSet()
	inputProps := make(map[string]pschema.PropertySpec)
	for _, param := range pathItem.Get.Parameters {
		if param.Value.In != parameterLocationPath {
			continue
		}

		paramName := param.Value.Name
		inputProps[paramName] = pschema.PropertySpec{
			Description: param.Value.Description,
			TypeSpec:    pschema.TypeSpec{Type: "string"},
		}
		requiredInputs.Add(paramName)
	}

	outputPropType, err := funcPkgCtx.propertyTypeSpec(parentName, returnTypeSchema)
	if err != nil {
		panic(err)
	}

	return pschema.FunctionSpec{
		Description: pathItem.Description,
		Inputs: &pschema.ObjectTypeSpec{
			Properties: inputProps,
			Required:   requiredInputs.SortedValues(),
		},
		Outputs: &pschema.ObjectTypeSpec{
			Properties: map[string]pschema.PropertySpec{
				"items": {
					TypeSpec: *outputPropType,
				},
			},
			Required: []string{"items"},
		},
	}
}

// gatherResource generates a resource spec from a POST API endpoint schema and
// adds it to the Pulumi schema spec.
func (o *OpenAPIContext) gatherResource(
	apiPath string,
	resourceRequestType openapi3.Schema,
	resourceResponseType *openapi3.Schema,
	pathParams openapi3.Parameters,
	module string) error {
	var resourceTypeToken *string
	var err error

	if resourceRequestType.Discriminator != nil {
		for _, mappingRef := range resourceRequestType.Discriminator.Mapping {
			schemaName := strings.TrimPrefix(mappingRef, componentsSchemaRefPrefix)
			typeSchema, ok := o.Doc.Components.Schemas[schemaName]
			if !ok {
				return errors.Errorf("%s not found in api schemas for discriminated type in path %s", schemaName, apiPath)
			}

			ensureRequestSchemaTitle(schemaName, typeSchema)
			resourceTypeToken, err = o.gatherResourceProperties(*typeSchema.Value, resourceResponseType, apiPath, module)
		}
	} else {
		resourceTypeToken, err = o.gatherResourceProperties(resourceRequestType, resourceResponseType, apiPath, module)
	}

	if err != nil {
		return errors.Wrapf(err, "gathering resource from api path %s", apiPath)
	}

	resourceSpec := o.Pkg.Resources[*resourceTypeToken]
	requiredInputs := codegen.NewStringSet(resourceSpec.RequiredInputs...)

	// If this endpoint path has path parameters,
	// then those should be required inputs too.
	for _, param := range pathParams {
		if param.Value.In != parameterLocationPath {
			continue
		}

		paramName := param.Value.Name
		resourceSpec.InputProperties[paramName] = pschema.PropertySpec{
			Description: param.Value.Description,
			TypeSpec:    pschema.TypeSpec{Type: "string"},
		}
		requiredInputs.Add(paramName)
	}

	o.Pkg.Resources[*resourceTypeToken] = resourceSpec

	return nil
}

// gatherResourceProperties generates a resource spec's input and output properties
// based on its API schema. Returns the Pulumi type token for the newly-added resource.
func (o *OpenAPIContext) gatherResourceProperties(requestBodySchema openapi3.Schema, responseBodySchema *openapi3.Schema, apiPath, module string) (*string, error) {
	pkgCtx := &resourceContext{
		mod:               module,
		pkg:               o.Pkg,
		resourceName:      requestBodySchema.Title,
		openapiComponents: *o.Doc.Components,
		visitedTypes:      codegen.NewStringSet(),
	}

	inputProperties := make(map[string]pschema.PropertySpec)
	properties := make(map[string]pschema.PropertySpec)
	requiredInputs := codegen.NewStringSet()
	requiredOutputs := codegen.NewStringSet()
	typeToken := fmt.Sprintf("%s:%s:%s", o.Pkg.Name, module, requestBodySchema.Title)

	for propName, prop := range requestBodySchema.Properties {
		var propSpec pschema.PropertySpec

		if prop.Value.AdditionalProperties.Has != nil {
			allowed := *prop.Value.AdditionalProperties.Has
			if allowed {
				// There's only ever going to be a single property
				// in the map, which will either have an inlined
				// properties schema or have a type ref. Either way,
				// the `propertyTypeSpec` method will take care of it.
				for _, v := range prop.Value.Properties {
					typeSpec, err := pkgCtx.propertyTypeSpec(propName, *v)
					if err != nil {
						return nil, errors.Wrapf(err, "generating additional properties type spec for %s (path: %s)", propName, apiPath)
					}

					propSpec = pschema.PropertySpec{
						TypeSpec: pschema.TypeSpec{
							Type:                 "object",
							AdditionalProperties: typeSpec,
						},
					}
				}
			} else {
				propSpec = pkgCtx.genPropertySpec(ToPascalCase(propName), *prop)
			}
		} else {
			propSpec = pkgCtx.genPropertySpec(ToPascalCase(propName), *prop)
		}

		// Skip read-only properties and `id` properties as direct inputs for resources.
		if !prop.Value.ReadOnly && propName != "id" {
			inputProperties[propName] = propSpec
		}

		// - All input properties must also be available as output
		// properties.
		// - Don't add `id` to the output properties since Pulumi
		// automatically adds that via `CustomResource` which
		// is what all resources in the SDK will extend.
		if propName != "id" {
			properties[propName] = propSpec
		}
	}

	if responseBodySchema != nil {
		for propName, prop := range responseBodySchema.Properties {
			var propSpec pschema.PropertySpec

			if prop.Value.AdditionalProperties.Has != nil {
				allowed := *prop.Value.AdditionalProperties.Has
				if allowed {
					// There's only ever going to be a single property
					// in the map, which will either have an inlined
					// properties schema or have a type ref. Either way,
					// the `propertyTypeSpec` method will take care of it.
					for _, v := range prop.Value.Properties {
						typeSpec, err := pkgCtx.propertyTypeSpec(propName, *v)
						if err != nil {
							return nil, errors.Wrapf(err, "generating additional properties type spec for %s (path: %s)", propName, apiPath)
						}

						propSpec = pschema.PropertySpec{
							TypeSpec: pschema.TypeSpec{
								Type:                 "object",
								AdditionalProperties: typeSpec,
							},
						}
					}
				} else {
					propSpec = pkgCtx.genPropertySpec(ToPascalCase(propName), *prop)
				}
			} else {
				propSpec = pkgCtx.genPropertySpec(ToPascalCase(propName), *prop)
			}

			// Don't add `id` to the output properties since Pulumi
			// automatically adds that via `CustomResource` which
			// is what all resources in the SDK will extend.
			if propName != "id" {
				properties[propName] = propSpec
			}
		}
	}

	// Create a set of required inputs for this resource.
	// Filter out required props that are marked as read-only.
	for _, requiredProp := range requestBodySchema.Required {
		propSchema := requestBodySchema.Properties[requiredProp]

		// If the required property's schema is not found,
		// it's likely that the OpenAPI doc lists the
		// required props that belong to some referenced
		// type. So ignore this.
		if propSchema == nil {
			glog.Warningf("Schema not found for required property: %s (type: %s)", requiredProp, requestBodySchema.Title)
			continue
		}

		// `name` property is not strictly required as Pulumi can auto-name it
		// based on the Pulumi resource name.
		if propSchema.Value.ReadOnly {
			continue
		}

		if requiredProp == "name" {
			if autoNameProp, ok := o.autoNameMap[typeToken]; ok {
				return nil, errors.Errorf("auto-name prop already exists for resource %s (existing: %s, new: %s)", typeToken, autoNameProp, requiredProp)
			}
			o.autoNameMap[typeToken] = "name"

			continue
		}

		requiredInputs.Add(requiredProp)
	}

	// Create a set of required outputs.
	// Use the `Required` property of the request body schema,
	// instead of `requiredInputs` sorted set because the `Required`
	// properties in the OpenAPI spec could all be marked as
	// read-only in which case, they wouldn't have been
	// added to the `requiredInputs` set.
	for _, required := range requestBodySchema.Required {
		requiredOutputs.Add(required)
	}
	// If there is a response body schema, then add its required
	// properties as well.
	if responseBodySchema != nil {
		for _, required := range responseBodySchema.Required {
			requiredOutputs.Add(required)
		}
	}

	if len(requestBodySchema.AllOf) > 0 {
		parentName := ToPascalCase(requestBodySchema.Title)
		var types []pschema.TypeSpec
		for _, schemaRef := range requestBodySchema.AllOf {
			if schemaRef == nil || schemaRef.Value == nil || schemaRef.Value.Type != "object" {
				continue
			}

			typ, err := pkgCtx.propertyTypeSpec(parentName, *schemaRef)
			if err != nil {
				return nil, errors.Wrapf(err, "generating property type spec from allOf schema for %s", requestBodySchema.Title)
			}
			types = append(types, *typ)
		}

		// Now that all of the types have been added to schema's Types,
		// gather all of their properties and smash them together into
		// a new type and get rid of those top-level ones.
		typeToken := fmt.Sprintf("%s:%s:%s", o.Pkg.Name, module, parentName)
		for _, t := range types {
			refTypeTok := strings.TrimPrefix(t.Ref, "#/types/")
			refType := pkgCtx.pkg.Types[refTypeTok]

			for name, propSpec := range refType.Properties {
				inputProperties[name] = propSpec
				properties[name] = propSpec
			}

			for _, r := range refType.Required {
				if requiredInputs.Has(r) {
					continue
				}
				requiredInputs.Add(r)
			}

			pkgCtx.visitedTypes.Delete(refTypeTok)
			delete(pkgCtx.pkg.Types, refTypeTok)
		}

		if existing, ok := o.resourceCRUDMap[typeToken]; ok {
			existing.C = &apiPath
		} else {
			o.resourceCRUDMap[typeToken] = &CRUDOperationsMap{
				C: &apiPath,
			}
		}

		o.Pkg.Resources[typeToken] = pschema.ResourceSpec{
			ObjectTypeSpec: pschema.ObjectTypeSpec{
				Description: requestBodySchema.Description,
				Type:        "object",
				Properties:  properties,
				Required:    requiredOutputs.SortedValues(),
			},
			InputProperties: inputProperties,
			RequiredInputs:  requiredInputs.SortedValues(),
		}

		return &typeToken, nil
	}

	if existing, ok := o.resourceCRUDMap[typeToken]; ok {
		existing.C = &apiPath
	} else {
		o.resourceCRUDMap[typeToken] = &CRUDOperationsMap{
			C: &apiPath,
		}
	}

	o.Pkg.Resources[typeToken] = pschema.ResourceSpec{
		ObjectTypeSpec: pschema.ObjectTypeSpec{
			Description: requestBodySchema.Description,
			Type:        "object",
			Properties:  properties,
			Required:    requiredOutputs.SortedValues(),
		},
		InputProperties: inputProperties,
		RequiredInputs:  requiredInputs.SortedValues(),
	}

	return &typeToken, nil
}

// genPropertySpec returns a property spec from an schema ref.
// The type spec of the returned property spec can be any of the
// supported types in Pulumi, including ref's to other types
// within the schema. In the case of ref's to other types, those
// other types are automatically added to the Pulumi schema spec's
// `Types` property.
func (ctx *resourceContext) genPropertySpec(propName string, p openapi3.SchemaRef) pschema.PropertySpec {
	propertySpec := pschema.PropertySpec{
		Description: p.Value.Description,
	}
	if p.Value.Default != nil {
		propertySpec.Default = p.Value.Default
	}
	languageName := strings.ToUpper(propName[:1]) + propName[1:]
	if languageName == ctx.resourceName {
		// .NET does not allow properties to be the same as the enclosing class - so special case these
		propertySpec.Language = map[string]pschema.RawMessage{
			"csharp": rawMessage(dotnetgen.CSharpPropertyInfo{
				Name: languageName + "Value",
			}),
		}
	}
	// JSONSchema type includes `$ref` and `$schema` properties, and $ is an invalid character in
	// the generated names. Replace them with `Ref` and `Schema`.
	if strings.HasPrefix(propName, "$") {
		propertySpec.Language = map[string]pschema.RawMessage{
			"csharp": rawMessage(dotnetgen.CSharpPropertyInfo{
				Name: strings.ToUpper(propName[1:2]) + propName[2:],
			}),
		}
	}

	typeSpec, err := ctx.propertyTypeSpec(propName, p)
	if err != nil {
		contract.Failf("Failed to generate type spec (resource: %s, prop %s): %v", ctx.resourceName, propName, err)
	}

	propertySpec.TypeSpec = *typeSpec

	return propertySpec
}

// propertyTypeSpec converts an API schema to a Pulumi property type spec.
func (ctx *resourceContext) propertyTypeSpec(parentName string, propSchema openapi3.SchemaRef) (*pschema.TypeSpec, error) {
	// References to other type definitions as long as the type is not an array.
	// Arrays and enums will be handled later in this method.
	if propSchema.Ref != "" && propSchema.Value.Type != arrayType && len(propSchema.Value.Enum) == 0 {
		schemaName := strings.TrimPrefix(propSchema.Ref, componentsSchemaRefPrefix)
		typName := ToPascalCase(schemaName)
		tok := fmt.Sprintf("%s:%s:%s", ctx.pkg.Name, ctx.mod, typName)

		typeSchema, ok := ctx.openapiComponents.Schemas[schemaName]
		if !ok {
			return nil, errors.Errorf("definition %s not found in resource %s", schemaName, parentName)
		}

		if !ctx.visitedTypes.Has(tok) {
			ctx.visitedTypes.Add(tok)
			specs, requiredSpecs, err := ctx.genProperties(typName, *typeSchema.Value)
			if err != nil {
				return nil, errors.Wrapf(err, "generating properties for %s", typName)
			}

			ctx.pkg.Types[tok] = pschema.ComplexTypeSpec{
				ObjectTypeSpec: pschema.ObjectTypeSpec{
					Description: typeSchema.Value.Description,
					Type:        "object",
					Properties:  specs,
					Required:    requiredSpecs.SortedValues(),
				},
			}
		}

		referencedTypeName := fmt.Sprintf("#/types/%s", tok)
		return &pschema.TypeSpec{Ref: referencedTypeName}, nil
	}

	// Inline properties.
	if len(propSchema.Value.Properties) > 0 {
		typName := parentName + "Properties"
		tok := fmt.Sprintf("%s:%s:%s", ctx.pkg.Name, ctx.mod, typName)
		specs, requiredSpecs, err := ctx.genProperties(typName, *propSchema.Value)
		if err != nil {
			return nil, err
		}

		ctx.pkg.Types[tok] = pschema.ComplexTypeSpec{
			ObjectTypeSpec: pschema.ObjectTypeSpec{
				Description: propSchema.Value.Description,
				Type:        "object",
				Properties:  specs,
				Required:    requiredSpecs.SortedValues(),
			},
		}
		referencedTypeName := fmt.Sprintf("#/types/%s", tok)
		return &pschema.TypeSpec{Ref: referencedTypeName}, nil
	}

	// Union types.
	if len(propSchema.Value.OneOf) > 0 {
		var types []pschema.TypeSpec
		for _, schemaRef := range propSchema.Value.OneOf {
			typ, err := ctx.propertyTypeSpec(parentName, *schemaRef)
			if err != nil {
				return nil, err
			}
			types = append(types, *typ)
		}

		var discriminator *pschema.DiscriminatorSpec
		if propSchema.Value.Discriminator != nil {
			discriminator = &pschema.DiscriminatorSpec{
				PropertyName: ToSdkName(propSchema.Value.Discriminator.PropertyName),
			}

			mapping := make(map[string]string)
			for discriminatorProperyValue, apiSchemaRef := range propSchema.Value.Discriminator.Mapping {
				resourceTypeName := strings.TrimPrefix(apiSchemaRef, "#/components/schemas/")
				resourceTypeName = ToPascalCase(resourceTypeName)
				for _, typeSpec := range types {
					if !strings.Contains(typeSpec.Ref, resourceTypeName) {
						continue
					}
					mapping[discriminatorProperyValue] = typeSpec.Ref
				}
			}
			discriminator.Mapping = mapping
		}

		return &pschema.TypeSpec{
			OneOf:         types,
			Discriminator: discriminator,
		}, nil
	}

	if len(propSchema.Value.AllOf) > 0 {
		properties, requiredPropSpecs, err := ctx.genPropertiesFromAllOf(parentName, propSchema.Value.AllOf)
		if err != nil {
			return nil, errors.Wrap(err, "generating properties from allOf schema definition")
		}

		tok := fmt.Sprintf("%s:%s:%s", ctx.pkg.Name, ctx.mod, ToPascalCase(parentName))
		ctx.pkg.Types[tok] = pschema.ComplexTypeSpec{
			ObjectTypeSpec: pschema.ObjectTypeSpec{
				Description: propSchema.Value.Description,
				Type:        "object",
				Properties:  properties,
				Required:    requiredPropSpecs.SortedValues(),
			},
		}

		return &pschema.TypeSpec{
			Ref: fmt.Sprintf("#/types/%s", tok),
		}, nil
	}

	if len(propSchema.Value.Enum) > 0 {
		enum, err := ctx.genEnumType(parentName, *propSchema.Value)
		if err != nil {
			return nil, errors.Wrapf(err, "generating enum for %s", parentName)
		}
		if enum != nil {
			return enum, nil
		}
	}

	// All other types.
	switch propSchema.Value.Type {
	case openapi3.TypeInteger:
		return &pschema.TypeSpec{Type: "integer"}, nil
	case openapi3.TypeString:
		return &pschema.TypeSpec{Type: "string"}, nil
	case openapi3.TypeBoolean:
		return &pschema.TypeSpec{Type: "boolean"}, nil
	case openapi3.TypeNumber:
		return &pschema.TypeSpec{Type: "number"}, nil
	case openapi3.TypeObject:
		return &pschema.TypeSpec{Ref: "pulumi.json#/Any"}, nil
	case openapi3.TypeArray:
		elementType, err := ctx.propertyTypeSpec(parentName+"Item", *propSchema.Value.Items)
		if err != nil {
			return nil, err
		}
		return &pschema.TypeSpec{
			Type:  arrayType,
			Items: elementType,
		}, nil
	}

	return nil, errors.Errorf("failed to generate property types for %+v", *propSchema.Value)
}

// genProperties returns a map of the property names and their corresponding
// property type spec and the required properties as a sorted set.
func (ctx *resourceContext) genProperties(parentName string, typeSchema openapi3.Schema) (map[string]pschema.PropertySpec, codegen.StringSet, error) {
	specs := map[string]pschema.PropertySpec{}
	requiredSpecs := codegen.NewStringSet()

	for _, name := range codegen.SortedKeys(typeSchema.Properties) {
		value := typeSchema.Properties[name]
		sdkName := ToSdkName(name)

		var typeSpec *pschema.TypeSpec
		var err error

		if value.Value.AdditionalProperties.Has != nil {
			allowed := *value.Value.AdditionalProperties.Has
			if allowed {
				// There's only ever going to be a single property
				// in the map, which will either have an inlined
				// properties schema or have a type ref. Either way,
				// the `propertyTypeSpec` method will take care of it.
				for _, v := range value.Value.Properties {
					addlPropsTypeSpec, err := ctx.propertyTypeSpec(sdkName, *v)
					if err != nil {
						return nil, nil, errors.Wrapf(err, "generating additional properties type spec for %s (parentName: %s)", sdkName, parentName)
					}

					typeSpec = &pschema.TypeSpec{
						Type:                 "object",
						AdditionalProperties: addlPropsTypeSpec,
					}
				}
			} else {
				typeSpec, err = ctx.propertyTypeSpec(parentName+ToPascalCase(name), *value)
				if err != nil {
					return nil, nil, errors.Wrapf(err, "property %s", name)
				}
			}
		} else {
			typeSpec, err = ctx.propertyTypeSpec(parentName+ToPascalCase(name), *value)
			if err != nil {
				return nil, nil, errors.Wrapf(err, "property %s", name)
			}
		}

		propertySpec := pschema.PropertySpec{
			Description: value.Value.Description,
			TypeSpec:    *typeSpec,
		}

		// Don't set default values for array-type properties
		// since Pulumi doesn't support it and also it isn't
		// very helpful anyway for arrays.
		if value.Value.Default != nil && value.Value.Type != arrayType {
			propertySpec.Default = value.Value.Default
		}

		specs[sdkName] = propertySpec
	}

	for _, name := range typeSchema.Required {
		sdkName := ToSdkName(name)
		if _, has := specs[sdkName]; has {
			requiredSpecs.Add(sdkName)
		}
	}

	if len(typeSchema.AllOf) > 0 {
		return ctx.genPropertiesFromAllOf(parentName, typeSchema.AllOf)
	}

	return specs, requiredSpecs, nil
}

// genPropertiesFromAllOf returns a map of property names and their corresponding
// property type spec gathered from a type's allOf schema.
func (ctx *resourceContext) genPropertiesFromAllOf(parentName string, allOf openapi3.SchemaRefs) (map[string]pschema.PropertySpec, codegen.StringSet, error) {
	var types []pschema.TypeSpec
	for _, schemaRef := range allOf {
		if schemaRef.Ref == "" && schemaRef.Value.Type != "object" {
			glog.Warningf("Prop type %s uses allOf schema but one of the schema refs is invalid", parentName)
			continue
		}

		typ, err := ctx.propertyTypeSpec(parentName, *schemaRef)
		if err != nil {
			return nil, nil, err
		}
		types = append(types, *typ)
	}

	// Now that all of the types have been added to schema's Types,
	// gather all of their properties and smash them together into
	// a new type.
	properties := make(map[string]pschema.PropertySpec)
	requiredSpecs := codegen.NewStringSet()
	for _, t := range types {
		refTypeTok := strings.TrimPrefix(t.Ref, "#/types/")
		refType := ctx.pkg.Types[refTypeTok]

		for name, propSpec := range refType.Properties {
			properties[name] = propSpec
		}

		for _, r := range refType.Required {
			if requiredSpecs.Has(r) {
				continue
			}
			requiredSpecs.Add(r)
		}

		ctx.visitedTypes.Delete(refTypeTok)
		delete(ctx.pkg.Types, refTypeTok)
	}

	return properties, requiredSpecs, nil
}

func getStringEnumValues(rawEnumValues []interface{}) ([]pschema.EnumValueSpec, codegen.StringSet) {
	enums := make([]pschema.EnumValueSpec, 0)
	names := codegen.NewStringSet()

	for _, val := range rawEnumValues {
		name := ToPascalCase(val.(string))
		if names.Has(name) {
			continue
		}

		names.Add(name)
		enumVal := pschema.EnumValueSpec{
			Value: val,
			Name:  name,
		}
		enums = append(enums, enumVal)
	}

	return enums, names
}

func getIntegerEnumValues(rawEnumValues []interface{}) ([]pschema.EnumValueSpec, codegen.StringSet) {
	enums := make([]pschema.EnumValueSpec, 0)
	names := codegen.NewStringSet()

	for _, val := range rawEnumValues {
		name := fmt.Sprintf("%d", val)
		enumVal := pschema.EnumValueSpec{
			Value: val,
			Name:  name,
		}
		names.Add(name)
		enums = append(enums, enumVal)
	}

	return enums, names
}

// genEnumType generates the enum type for a given schema.
func (ctx *resourceContext) genEnumType(enumName string, propSchema openapi3.Schema) (*pschema.TypeSpec, error) {
	if len(propSchema.Type) == 0 {
		return nil, nil
	}

	typName := ToPascalCase(enumName)
	tok := fmt.Sprintf("%s:%s:%s", ctx.pkg.Name, ctx.mod, typName)

	enumSpec := &pschema.ComplexTypeSpec{
		ObjectTypeSpec: pschema.ObjectTypeSpec{
			Description: propSchema.Description,
			Type:        propSchema.Type,
		},
	}

	var names codegen.StringSet

	switch propSchema.Type {
	case openapi3.TypeString:
		enumSpec.Enum, names = getStringEnumValues(propSchema.Enum)
	case openapi3.TypeInteger:
		enumSpec.Enum, names = getIntegerEnumValues(propSchema.Enum)
	default:
		return nil, errors.Errorf("cannot handle enum values of type %s", propSchema.Type)
	}

	referencedTypeName := fmt.Sprintf("#/types/%s", tok)

	// Make sure that the type name we composed doesn't clash with another type
	// already defined in the schema earlier. The same enum does show up in multiple
	// places of specs, so we want to error only if they a) have the same name
	// b) the list of values does not match.
	if other, ok := ctx.pkg.Types[tok]; ok {
		same := len(enumSpec.Enum) == len(other.Enum)
		for _, val := range other.Enum {
			same = same && names.Has(val.Name)
		}

		if !same {
			msg := fmt.Sprintf("duplicate enum %q: %+v vs. %+v", tok, enumSpec.Enum, other.Enum)
			return nil, &duplicateEnumError{msg: msg}
		}

		return &pschema.TypeSpec{
			Ref: referencedTypeName,
		}, nil
	}

	ctx.pkg.Types[tok] = *enumSpec

	return &pschema.TypeSpec{
		Ref: referencedTypeName,
	}, nil
}
