package controller

import (
	"errors"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

func businessQueryFilters(c *gin.Context, keysOnly bool) model.BusinessLogFilter {
	fields := []string{"tag_level_1", "tag_level_2"}
	if !keysOnly {
		fields = append(fields, "business_system_id", "business_id", "external_user_id", "external_task_id", "async_task_id")
	}
	filters := model.BusinessLogFilter{}
	for _, field := range fields {
		if c.Request.URL.Query().Has(field) {
			filters[field] = strings.TrimSpace(c.Query(field))
		}
	}
	return filters
}

func normalizeBusinessTokenInput(token *model.Token, request tokenRequest) error {
	for _, label := range []struct {
		input  *string
		target *string
	}{{request.TagLevel1, &token.TagLevel1}, {request.TagLevel2, &token.TagLevel2}} {
		if label.input == nil {
			continue
		}
		value, err := model.NormalizeBusinessIdentifier(*label.input, 64)
		if err != nil {
			return err
		}
		*label.target = value
	}
	if request.AllowedModalities != nil {
		return token.SetAllowedModalities(*request.AllowedModalities)
	}
	return nil
}

func GetBusinessProfile(c *gin.Context) {
	user, err := model.GetUserById(c.GetInt("id"), false)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, gin.H{"business_system_id": user.BusinessSystemID})
}

func UpdateBusinessProfile(c *gin.Context) {
	var request struct {
		BusinessSystemID *string `json:"business_system_id"`
	}
	if err := common.DecodeJson(c.Request.Body, &request); err != nil {
		common.ApiError(c, err)
		return
	}
	if request.BusinessSystemID == nil {
		common.ApiError(c, errors.New("business_system_id is required"))
		return
	}
	if err := model.UpdateBusinessSystemID(c.GetInt("id"), *request.BusinessSystemID); err != nil {
		common.ApiError(c, err)
		return
	}
	GetBusinessProfile(c)
}
