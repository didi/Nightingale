package router

import (
	"github.com/ccfos/nightingale/v6/models"
	"github.com/ccfos/nightingale/v6/pkg/flashduty"
	"github.com/ccfos/nightingale/v6/pkg/ormx"
	"github.com/ccfos/nightingale/v6/pkg/secu"
	"github.com/ccfos/nightingale/v6/tool"

	"github.com/gin-gonic/gin"
	"github.com/toolkits/pkg/ginx"
	"github.com/toolkits/pkg/logger"
)

func (rt *Router) selfProfileGet(c *gin.Context) {
	user := c.MustGet("user").(*models.User)
	if user.IsAdmin() {
		user.Admin = true
	}
	ginx.NewRender(c).Data(user, nil)
}

type selfProfileForm struct {
	Nickname string       `json:"nickname"`
	Phone    string       `json:"phone"`
	Email    string       `json:"email"`
	Portrait string       `json:"portrait"`
	Contacts ormx.JSONObj `json:"contacts"`
}

func (rt *Router) selfProfilePut(c *gin.Context) {
	var f selfProfileForm
	ginx.BindJSON(c, &f)

	user := c.MustGet("user").(*models.User)
	oldInfo := models.User{
		Username: user.Username,
		Phone:    user.Phone,
		Email:    user.Email,
	}
	user.Nickname = f.Nickname
	user.Phone = f.Phone
	user.Email = f.Email
	user.Portrait = f.Portrait
	user.Contacts = f.Contacts
	user.UpdateBy = user.Username

	if flashduty.NeedSyncUser(rt.Ctx) {
		flashduty.UpdateUser(rt.Ctx, oldInfo, f.Email, f.Phone)
	}

	ginx.NewRender(c).Message(user.UpdateAllFields(rt.Ctx))
}

type selfPasswordForm struct {
	OldPass string `json:"oldpass" binding:"required"`
	NewPass string `json:"newpass" binding:"required"`
}

func (rt *Router) selfPasswordPut(c *gin.Context) {
	var f selfPasswordForm
	ginx.BindJSON(c, &f)
	user := c.MustGet("user").(*models.User)

	newPassWord := f.NewPass
	oldPassWord := f.OldPass
	if rt.HTTP.RSA.OpenRSA {
		var err error
		newPassWord, err = secu.Decrypt(f.NewPass, rt.HTTP.RSA.RSAPrivateKey, rt.HTTP.RSA.RSAPassWord)
		if err != nil {
			logger.Errorf("RSA Decrypt failed: %v username: %s", err, user.Username)
			ginx.NewRender(c).Message(err)
			return
		}

		oldPassWord, err = secu.Decrypt(f.OldPass, rt.HTTP.RSA.RSAPrivateKey, rt.HTTP.RSA.RSAPassWord)
		if err != nil {
			logger.Errorf("RSA Decrypt failed: %v username: %s", err, user.Username)
			ginx.NewRender(c).Message(err)
			return
		}
	}

	ginx.NewRender(c).Message(user.ChangePassword(rt.Ctx, oldPassWord, newPassWord))
}

type tokenForm struct {
	TokenName string `json:"token_name"`
	Token     string `json:"token"`
}

func (rt *Router) getToken(c *gin.Context) {
	username := c.MustGet("username").(string)
	tokens, err := models.GetTokensByUsername(rt.Ctx, username)
	if err != nil {
		ginx.NewRender(c).Message(err)
		return
	}

	ginx.NewRender(c).Data(tokens, nil)
}

func (rt *Router) addToken(c *gin.Context) {
	var f tokenForm
	ginx.BindJSON(c, &f)

	username := c.MustGet("username").(string)

	tokens, err := models.GetTokensByUsername(rt.Ctx, username)
	if err != nil {
		ginx.NewRender(c).Message(err)
		return
	}
	if len(tokens) >= 2 {
		ginx.NewRender(c).Message("token count exceeds the limit 2")
		return
	}

	for _, token := range tokens {
		if token.TokenName == f.TokenName {
			ginx.NewRender(c).Message("token name already exists")
			return
		}
	}

	token, err := models.AddToken(rt.Ctx, username, tool.GenToken(username), f.TokenName)
	ginx.NewRender(c).Data(token, err)
}

func (rt *Router) deleteToken(c *gin.Context) {
	var f tokenForm
	ginx.BindJSON(c, &f)

	username := c.MustGet("username").(string)
	tokenCount, err := models.CountToken(rt.Ctx, username)
	if err != nil {
		ginx.NewRender(c).Message(err)
		return
	}
	if tokenCount <= 1 {
		ginx.NewRender(c).Message("cannot delete the last token")
		return
	}

	ginx.NewRender(c).Message(models.DeleteToken(rt.Ctx, f.Token))
}
