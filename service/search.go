package service

import (
	"encoding/json"
	"fmt"
	"github.com/aosfather/bingo"
	"github.com/aosfather/bingo/utils"
	"github.com/go-redis/redis"
	"strconv"
	"crypto/md5"
	"time"
	"strings"
)

/**
  搜索实现
  通过倒排实现关键信息的实现
  规则：
   1、原始内容使用hashmap存储，对象【ID，Content】，键值 indexname，二级key根据md5 对象转json字符串
   2、针对原始内容带的标签，key value，生成set，名称为 indexname_key_value的形式，set内容放 通过内容md5出来的键值
   3、搜索的时候，根据传递的搜索条件 key，value 数组，对找到的set（形如indexname_key_value ），实行找交集
   4、根据交集的结果二级key，并从indexname的hashmap中获取json内容

*/

type Field struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type PageSearchResult struct {
	Id string `json:"uuid"`    //查询的请求id
	Index int64 `json:"page"`  //页码
	Data []TargetObject
}

type TargetObject struct {
	Id   string `json:"id"`
	Data json.RawMessage `json:"data"`
}
type SourceObject struct {
	TargetObject
	Fields map[string]string `json:"fields"`
}

type SearchEngine struct {
	indexs map[string]*searchIndex
	client *redis.Client
	logger utils.Log
	pageSize int64
}

func (this *SearchEngine) Init(context *bingo.ApplicationContext) {
	fmt.Println("init .....")
	db, err := strconv.Atoi(context.GetPropertyFromConfig("service.search.db"))
	if err != nil {
		db = 0
	}

	size, err := strconv.Atoi(context.GetPropertyFromConfig("service.search.pagesize"))
	if err != nil {
		size = 20 //默认大小20条
	}
	this.pageSize=int64(size)

	this.client = redis.NewClient(&redis.Options{
		Addr:     context.GetPropertyFromConfig("service.search.redis"),
		Password: "", // no password set
		DB:       db,
	})
	fmt.Println(context.GetPropertyFromConfig("service.search.redis"))
	this.indexs = make(map[string]*searchIndex)
	this.logger = context.GetLog("bingo_search")
}

func (this *SearchEngine) CreateIndex(name string) *searchIndex {
	if name != "" {
		index := this.indexs[name]
		if index == nil {
			index = &searchIndex{name, this}
			this.indexs[name] = index
		}
		return index
	}

	return nil
}

func (this *SearchEngine) LoadSource(name string, obj *SourceObject) {

	index := this.CreateIndex(name)
	if index != nil {
		index.LoadObject(obj)
	}

}

func (this *SearchEngine) FetchByPage(request string,page int64) *PageSearchResult {
	if request != "" {
		//获取request的name
		index:=strings.Index(request,":")
		print(index)
		if index<0 {
			this.logger.Error("pagerequest's index name not found !")
			return nil  //找不到对应的索引类型
		}

		name:=request[0:index]
		if page<=0 {
			page=1
		}
		startIndex:=(page-1) * this.pageSize+1  //从1开始计数
		endIndex:=   page*this.pageSize

		keys,err:=this.client.LRange(request,startIndex,endIndex).Result()
		if err!=nil {
			this.logger.Debug("no content by page!")
			return nil
		}

		return &PageSearchResult{request,page,this.fetch(name,keys...)}
	}

	return nil
}

func (this *SearchEngine)createRequst(name string,keys... string) string {
    key:= getSearchRequestUuid(name)
	var datas []interface{}

	for _,v:=range keys {
		datas=append(datas,v)
	}

	this.client.LPush(key,datas...)
	fmt.Println("%v",datas)
    this.client.Expire(key,time.Duration(30)*time.Minute)//30分钟后失效
	return key
}
//获取内容
func (this *SearchEngine) fetch(name string,keys ... string) []TargetObject{
	datas, err1 := this.client.HMGet(name, keys...).Result()
	if err1 == nil && len(datas) > 0{

		var targets []TargetObject

		for _, v := range datas {
			if v != nil {
				t := TargetObject{}
				json.Unmarshal([]byte(fmt.Sprintf("%v", v)), &t)
				targets = append(targets, t)
			}
		}

		return targets

	} else {
		this.logger.Error("get data by index error!%s", err1.Error())
	}

	return nil
}


func (this *SearchEngine) Search(name string, input ...Field) *PageSearchResult {
	if name != "" {
		index := this.indexs[name]
		if index != nil {
			r,data:= index.Search(input...)
			return &PageSearchResult{r,1,data}

		}
		this.logger.Info("not found index %s", name)
	}

	return nil
}

type searchIndex struct {
	name   string
	engine *SearchEngine
}

//搜索信息
func (this *searchIndex) Search(input ...Field) (string,[]TargetObject) {
	//搜索索引
	var searchkeys []string
	for _, f := range input {
		searchkeys = append(searchkeys, this.buildTheKey(f))
	}
	//取交集
	result := this.engine.client.SInter(searchkeys...)
	targetkeys, err := result.Result()
	if err != nil {
		this.engine.logger.Error("inter key error!%s", err.Error())
		return "",nil
	}
	if len(targetkeys) > 0 {
		//生成request
		r:=this.engine.createRequst(this.name,targetkeys...)
		//写入到列表中
        query:=targetkeys[1:this.engine.pageSize]
		//根据最后的id，从data中取出所有命中的元素
		return r,this.engine.fetch(this.name,query...)
		//datas, err1 := this.engine.client.HMGet(this.name, targetkeys...).Result()
		//if err1 == nil && len(datas) > 0{
		//
		//		var targets []TargetObject
		//
		//		for _, v := range datas {
		//			if v != nil {
		//				t := TargetObject{}
		//				json.Unmarshal([]byte(fmt.Sprintf("%v", v)), &t)
		//				targets = append(targets, t)
		//			}
		//		}
		//
		//		return targets
		//
		//} else {
		//	this.engine.logger.Error("get data by index error!%s", err1.Error())
		//}

	}

	return "",nil
}

//刷新索引，加载信息到存储中
func (this *searchIndex) LoadObject(obj *SourceObject) {
	data, _ := json.Marshal(obj)
	key := getMd5str(string(data))
	//1、放入数据到目标集合中
	this.engine.client.HSet(this.name, key, string(data))

	//2、根据field存储到各个对应的索引中

	for k, v := range obj.Fields {
		this.engine.client.SAdd(this.buildTheKeyByItem(k,v), key)
	}

}

func (this *searchIndex) buildTheKey(f Field) string {
	return this.buildTheKeyByItem(f.Key, f.Value)
}

func (this *searchIndex) buildTheKeyByItem(key,value string) string {
	return fmt.Sprintf("%s_%s_%s", this.name, key, value)
}

func getMd5str(value string) string {
	data := []byte(value)
	has := md5.Sum(data)
	md5str1 := fmt.Sprintf("%x", has) //将[]byte转成16进制

	return md5str1

}

func getSearchRequestUuid(prefix string) string {
	return fmt.Sprintf("%s:%d",prefix, time.Now().UnixNano())
}