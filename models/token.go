package models

import (
	"github.com/ccfos/nightingale/v6/pkg/ctx"
)

type Token struct {
	Id        int64  `json:"id" gorm:"primaryKey"`
	Username  string `json:"username"`
	TokenName string `json:"token_name" gorm:"type:varchar(255)"`
	Token     string `json:"token" gorm:"type:varchar(255);uniqueIndex"`
}

func (Token) TableName() string {
	return "token"
}

func CountToken(ctx *ctx.Context, username string) (int64, error) {
	var count int64
	err := DB(ctx).Model(&Token{}).Where("username = ?", username).Count(&count).Error
	return count, err
}

func AddToken(ctx *ctx.Context, username, token, tokenName string) (*Token, error) {
	newToken := Token{
		TokenName: tokenName,
		Username:  username,
		Token:     token,
	}

	err := Insert(ctx, &newToken)
	return &newToken, err
}

func DeleteToken(ctx *ctx.Context, token string) error {
	return DB(ctx).Where("token = ?", token).Delete(&Token{}).Error
}

func GetTokensByUsername(ctx *ctx.Context, username string) ([]Token, error) {
	var tokens []Token
	err := DB(ctx).Where("username = ?", username).Find(&tokens).Error
	return tokens, err
}

func GetUserByToken(ctx *ctx.Context, token string) (*User, error) {
	var user User
	err := DB(ctx).Select("users.id, users.username").
		Joins("JOIN token t ON t.username = users.username").
		Where("t.token = ?", token).First(&user).Debug().Error
	return &user, err
}
