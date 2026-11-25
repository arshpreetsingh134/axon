package commands

import (
	"reflect"
	"testing"
)

func TestPrincipalsPerAudienceAreShortWithOwnKeys(t *testing.T) {
	id := devIdentity{ClientID: "shop.wfc", Sites: []string{"acme.shop.billing.app.billing", "acme.shop.product.app.product"}, PerAudience: true}
	got := id.principals()
	want := []principal{
		{ID: "shop.wfc--billing", KeyName: "billing.pem", Sites: []string{"acme.shop.billing.app.billing"}},
		{ID: "shop.wfc--product", KeyName: "product.pem", Sites: []string{"acme.shop.product.app.product"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("principals = %+v", got)
	}
	if x := id.extraKeys(); !reflect.DeepEqual(x, []string{"billing.pem", "product.pem"}) {
		t.Fatalf("extraKeys = %v", x)
	}
	single := devIdentity{ClientID: "shop.wfc", Sites: id.Sites}
	if p := single.principals(); len(p) != 1 || p[0].ID != "shop.wfc" || p[0].KeyName != "key.pem" || len(single.extraKeys()) != 0 {
		t.Fatalf("single principal = %+v", p)
	}
}
