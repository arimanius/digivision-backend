package productmeta

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/arimanius/digivision-backend/internal/rank"
	v1 "github.com/arimanius/digivision-backend/pkg/api/v1"
	"github.com/go-resty/resty/v2"
	"github.com/pkg/errors"
	"github.com/redis/go-redis/v9"
	"github.com/sirupsen/logrus"
	"strconv"
	"strings"
	"time"
)

type empty interface{}
type semaphore chan empty

type DigikalaFetcher struct {
	baseUrl     string
	apiBaseUrl  string
	client      *resty.Client
	redisClient *redis.Client
	maxRetry    int
	sem         semaphore
}

func NewDigikalaFetcher(
	baseUrl string, apiBaseUrl string, client *resty.Client, redisClient *redis.Client, maxRetry int, concurrencyFactor int,
) DigikalaFetcher {
	return DigikalaFetcher{
		baseUrl:     baseUrl,
		apiBaseUrl:  apiBaseUrl,
		client:      client,
		redisClient: redisClient,
		maxRetry:    maxRetry,
		sem:         make(semaphore, concurrencyFactor),
	}
}

func (f DigikalaFetcher) Fetch(ctx context.Context, product rank.Product) (*v1.Product, error) {
	pid, err := strconv.Atoi(product.Id)
	if err != nil {
		return nil, err
	}

	cache, err := f.redisClient.Get(ctx, fmt.Sprintf("product:%d", pid)).Result()
	dkProduct := DigikalaProduct{}
	if err == redis.Nil {
		err = f.fetchFromDigikala(ctx, product, &dkProduct)
	} else if err != nil {
		logrus.Error("failed to fetch product from redis ", pid, " err: ", err)
		err = f.fetchFromDigikala(ctx, product, &dkProduct)
	} else {
		err = json.Unmarshal([]byte(cache), &dkProduct)
	}

	if err != nil {
		return nil, err
	}
	p := dkProduct.Data.Product
	if p.IsInactive {
		return nil, fmt.Errorf("product %s is inactive", product.Id)
	}
	return &v1.Product{
		Id:       int32(pid),
		Title:    p.TitleFa,
		Url:      fmt.Sprintf("%s%s", f.baseUrl, p.Url.Uri),
		Status:   p.Status,
		ImageUrl: p.Images.Main.Url[0],
		Rate: &v1.Rating{
			Rate:  p.Rating.Rate,
			Count: p.Rating.Count,
		},
		Categories: ToCategories(f.baseUrl, p.Breadcrumb),
		Price:      p.DefaultVariant.Price.SellingPrice,
		Score:      product.Score,
	}, nil
}

func (f DigikalaFetcher) fetchFromDigikala(ctx context.Context, product rank.Product, dkProduct *DigikalaProduct) error {
	pid, err := strconv.Atoi(product.Id)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("%s/%d/", f.apiBaseUrl, pid)
	resp, err := f.client.R().SetContext(ctx).Get(url)
	if err != nil {
		return err
	}
	if resp.Status() != "200 OK" {
		return fmt.Errorf("failed to fetch product %s. status: %s", product.Id, resp.Status())
	}
	body := resp.Body()
	err = f.redisClient.Set(ctx, fmt.Sprintf("product:%d", pid), string(body), 0).Err()
	if err != nil {
		logrus.Errorf("failed to set product %d to redis. err: %s", pid, err)
	}
	err = json.Unmarshal(body, &dkProduct)
	return nil
}

type ProductWithError struct {
	Product *v1.Product
	Error   error
}

func (f DigikalaFetcher) AsyncFetch(ctx context.Context, products []rank.Product, count int) chan *ProductWithError {
	resp := make(chan *ProductWithError)

	responses := make([]chan *ProductWithError, len(products))
	for i := range products {
		responses[i] = make(chan *ProductWithError)
	}
	innerCtx, cancel := context.WithCancel(ctx)
	go func() {
		for i, product := range products {
			select {
			case <-innerCtx.Done():
				return
			default:
				f.singleAsyncFetch(innerCtx, product, responses[i])
			}
		}
	}()

	go func() {
		defer close(resp)
		defer cancel()
		c := 0
		i := 0
		for {
			if c >= count || i >= len(products) {
				return
			}
			select {
			case <-ctx.Done():
				return
			case p := <-responses[i]:
				if p == nil {
					return
				}
				i++
				resp <- p
				if p.Product != nil {
					c++
				}
			}
		}
	}()

	return resp
}

func (f DigikalaFetcher) singleAsyncFetch(ctx context.Context, product rank.Product, resp chan *ProductWithError) {
	var emp empty
	f.sem <- emp
	go func() {
		defer func() {
			<-f.sem
			close(resp)
		}()
		p, e := f.Fetch(ctx, product)
		retryCount := 0
		for e != nil {
			if strings.HasSuffix(e.Error(), "is inactive") {
				resp <- &ProductWithError{
					Product: nil,
					Error:   errors.Wrapf(e, "failed to fetch product %s", product.Id),
				}
				return
			}
			if strings.HasSuffix(e.Error(), "context canceled") {
				resp <- &ProductWithError{
					Product: nil,
					Error:   errors.Wrapf(e, "failed to fetch product %s", product.Id),
				}
				return
			}
			time.Sleep(1 * time.Second)
			if retryCount >= f.maxRetry {
				resp <- &ProductWithError{
					Product: nil,
					Error:   errors.Wrapf(e, "failed to fetch product %s after %d retries", product.Id, retryCount),
				}
				return
			}
			p, e = f.Fetch(ctx, product)
			retryCount++
		}
		resp <- &ProductWithError{
			Product: p,
			Error:   nil,
		}
	}()
}
