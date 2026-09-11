package handlers

import (
	"strings"

	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/bson"

	"github.com/karlo/masterdata-service/internal/models"
	"github.com/karlo/masterdata-service/internal/platform/response"
	"github.com/karlo/masterdata-service/internal/repository"
)

// CustomerHandler serves a company's own register of the people it delivers to.
//
// These are not platform users and never sign in: a customer is a consignee
// recorded so an order can name who is receiving the goods. Creating accounts
// for them would put thousands of identities in the IAM that nobody
// authenticates as.
type CustomerHandler struct {
	customers *repository.CustomerRepository
}

func NewCustomerHandler(c *repository.CustomerRepository) *CustomerHandler {
	return &CustomerHandler{customers: c}
}

// List pages the company's customers.
//
// @Summary  List customers
// @Tags     Customers
// @Security BearerAuth
// @Success  200 {array} models.Customer
// @Router   /customers [get]
func (h *CustomerHandler) List(c *gin.Context) {
	companyID, ok := callerCompany(c)
	if !ok {
		return
	}
	params := parseQuery(c, repository.CustomerFields())
	if badQuery(c, params) {
		return
	}

	rows, total, err := h.customers.List(c.Request.Context(), companyID, params)
	if err != nil {
		writeError(c, err)
		return
	}
	response.Paginated(c, rows, meta(params, total))
}

// Get returns one customer.
//
// @Summary  Get a customer
// @Tags     Customers
// @Security BearerAuth
// @Param    id path string true "Customer ID"
// @Success  200 {object} models.Customer
// @Router   /customers/{id} [get]
func (h *CustomerHandler) Get(c *gin.Context) {
	companyID, ok := callerCompany(c)
	if !ok {
		return
	}
	id, ok := pathObjectID(c)
	if !ok {
		return
	}
	row, err := h.customers.FindByID(c.Request.Context(), companyID, id)
	if err != nil {
		writeError(c, err)
		return
	}
	response.OK(c, row)
}

type customerRequest struct {
	Name         string `json:"name"`
	Code         string `json:"code"`
	NPWP         string `json:"npwp"`
	Address      string `json:"address"`
	ContactName  string `json:"contactName"`
	ContactPhone string `json:"contactPhone"`
	ContactEmail string `json:"contactEmail"`
}

// Create adds a customer.
//
// @Summary  Create a customer
// @Tags     Customers
// @Security BearerAuth
// @Success  201 {object} models.Customer
// @Router   /customers [post]
func (h *CustomerHandler) Create(c *gin.Context) {
	companyID, ok := callerCompany(c)
	if !ok {
		return
	}
	var body customerRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		response.BadRequest(c, "A customer needs a name")
		return
	}

	row := &models.Customer{
		// From the token, never the body. A customer id in a request body is
		// how one tenant writes into another's register.
		CompanyID:    companyID,
		Name:         strings.TrimSpace(body.Name),
		Code:         strings.TrimSpace(body.Code),
		NPWP:         strings.TrimSpace(body.NPWP),
		Address:      strings.TrimSpace(body.Address),
		ContactName:  strings.TrimSpace(body.ContactName),
		ContactPhone: strings.TrimSpace(body.ContactPhone),
		ContactEmail: strings.TrimSpace(body.ContactEmail),
	}
	if err := h.customers.Create(c.Request.Context(), row); err != nil {
		writeError(c, err)
		return
	}
	response.Created(c, row)
}

// Update changes a customer.
//
// @Summary  Update a customer
// @Tags     Customers
// @Security BearerAuth
// @Param    id path string true "Customer ID"
// @Success  200 {object} response.Envelope
// @Router   /customers/{id} [put]
func (h *CustomerHandler) Update(c *gin.Context) {
	companyID, ok := callerCompany(c)
	if !ok {
		return
	}
	id, ok := pathObjectID(c)
	if !ok {
		return
	}
	var body customerRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	// Only the fields actually sent. Building the update from every field would
	// blank whatever the client omitted, so a form that edits one value would
	// silently clear the rest.
	fields := bson.M{}
	set := func(key, value string) {
		if value != "" {
			fields[key] = strings.TrimSpace(value)
		}
	}
	set("name", body.Name)
	set("code", body.Code)
	set("npwp", body.NPWP)
	set("address", body.Address)
	set("contactName", body.ContactName)
	set("contactPhone", body.ContactPhone)
	set("contactEmail", body.ContactEmail)

	if err := h.customers.Update(c.Request.Context(), companyID, id, fields); err != nil {
		writeError(c, err)
		return
	}
	response.OKWithMessage(c, "Customer updated", nil)
}

// Delete removes a customer, keeping the record.
//
// @Summary  Delete a customer
// @Tags     Customers
// @Security BearerAuth
// @Param    id path string true "Customer ID"
// @Success  200 {object} response.Envelope
// @Router   /customers/{id} [delete]
func (h *CustomerHandler) Delete(c *gin.Context) {
	companyID, ok := callerCompany(c)
	if !ok {
		return
	}
	id, ok := pathObjectID(c)
	if !ok {
		return
	}
	if err := h.customers.SoftDelete(c.Request.Context(), companyID, id); err != nil {
		writeError(c, err)
		return
	}
	// Soft deleted: historical orders reference customers by id, and a hard
	// delete would render last year's invoice with a blank consignee.
	response.OKWithMessage(c, "Customer removed", nil)
}
