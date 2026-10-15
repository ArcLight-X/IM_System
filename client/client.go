package main

import (
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
)

type Client struct {
	ServerIP   string
	ServerPort int
	Name       string
	conn       net.Conn
	x          int
	closed     chan struct{}
}

func newClient(serverIP string, serverPort int) *Client {
	//创建客户端
	client := &Client{
		ServerIP:   serverIP,
		ServerPort: serverPort,
		closed:     make(chan struct{}),
	}
	//连接server
	conn, err := net.Dial("tcp", net.JoinHostPort(serverIP, strconv.Itoa(serverPort)))
	if err != nil {
		fmt.Println("net dial error")
		return nil
	}
	client.conn = conn
	return client
}

// 处理server回应的消息
func (client *Client) DealResponse() {
	io.Copy(os.Stdout, client.conn)
	close(client.closed)
}

func (client *Client) menu() bool {
	fmt.Println("1.公聊模式")
	fmt.Println("2.私聊模式")
	fmt.Println("3.更新用户名")
	fmt.Println("4.查询在线用户")
	fmt.Println("5.AI 问答")
	fmt.Println("6.群聊摘要")
	fmt.Println("0.退出")
	fmt.Scanln(&client.x)
	if client.x >= 0 && client.x <= 6 {
		return true
	} else {
		fmt.Println("输入不合法")
		return false
	}
}

// AI 问答
func (client *Client) AskAI() {
	var question string
	fmt.Println("请输入问题，exit退出")
	fmt.Scanln(&question)
	for question != "exit" {
		if len(question) != 0 {
			sendMsg := "/ai " + question + "\n"
			if _, err := client.conn.Write([]byte(sendMsg)); err != nil {
				fmt.Println("error:", err)
				return
			}
		}
		question = ""
		fmt.Println("请输入问题，exit退出")
		fmt.Scanln(&question)
	}
}

// 群聊摘要
func (client *Client) Summary() {
	sendMsg := "/summary 20\n"
	if _, err := client.conn.Write([]byte(sendMsg)); err != nil {
		fmt.Println("error:", err)
	}
}

// 公聊模式
func (client *Client) PublicChat() {
	var chatMsg string
	fmt.Println("请输入聊天内容,输入exit退出")
	fmt.Scanln(&chatMsg)

	for chatMsg != "exit" {
		if len(chatMsg) != 0 {
			sendMsg := chatMsg + "\n"
			_, err := client.conn.Write([]byte(sendMsg))
			if err != nil {
				fmt.Println("error:", err)
				break
			}
		}
		chatMsg = ""
		fmt.Println("请输入聊天内容,输入exit退出")
		fmt.Scanln(&chatMsg)
	}

}

// 查询在线用户
func (client *Client) Check() {
	sendMsg := "who\n"
	_, err := client.conn.Write([]byte(sendMsg))
	if err != nil {
		fmt.Println("error:", err)
		return
	}
}

// 私聊模式
func (client *Client) PrivateChat() {
	var remoteName string
	var chatMsg string
	client.Check()
	fmt.Println("请输入聊天对象用户名,exit退出")
	fmt.Scanln(&remoteName)
	for remoteName != "exit" {
		fmt.Println("请输入消息内容,exit退出")
		fmt.Scanln(&chatMsg)
		for chatMsg != "exit" {
			if len(chatMsg) != 0 {
				sendMsg := "to|" + remoteName + "|" + chatMsg + "\n"
				_, err := client.conn.Write([]byte(sendMsg))
				if err != nil {
					fmt.Println("error:", err)
					break
				}
			}
			chatMsg = ""
			fmt.Println("请输入消息内容,输入exit退出")
			fmt.Scanln(&chatMsg)
		}
		client.Check()
		fmt.Println("请输入聊天对象用户名,exit退出")
		fmt.Scanln(&remoteName)
	}
}

func (client *Client) UpdateName() bool {
	fmt.Println("请输入用户名")
	fmt.Scanln(&client.Name)

	sendMsg := "rename|" + client.Name + "\n"
	_, err := client.conn.Write([]byte(sendMsg))
	if err != nil {
		fmt.Println("error:", err)
		return false
	}
	return true

}

func (client *Client) isClosed() bool {
	select {
	case <-client.closed:
		return true
	default:
		return false
	}
}

func (client *Client) Run() {
	for {
		for client.menu() != true {
		}
		if client.isClosed() {
			fmt.Println("与服务器断开连接")
			return
		}
		switch client.x {
		case 1:
			fmt.Println("已选择公聊")
			client.PublicChat()
		case 2:
			fmt.Println("已选择私聊")
			client.PrivateChat()
		case 3:
			fmt.Println("已选择更改用户名")
			client.UpdateName()
		case 4:
			fmt.Println("已选择查询在线用户")
			client.Check()
		case 5:
			fmt.Println("已选择 AI 问答")
			client.AskAI()
		case 6:
			fmt.Println("已选择群聊摘要")
			client.Summary()
		case 0:
			return
		}
	}
}

var serverIP string
var serverPort int
var clientName string

func init() {
	flag.StringVar(&serverIP, "ip", "127.0.0.1", "设置服务器IP地址")
	flag.IntVar(&serverPort, "port", 8888, "设置服务器端口")
	flag.StringVar(&clientName, "name", "", "设置用户名")
}

func main() {
	flag.Parse()

	client := newClient(serverIP, serverPort)
	if client == nil {
		fmt.Println("连接服务器失败")
		return
	}
	go client.DealResponse()

	if clientName != "" {
		client.Name = clientName
		sendMsg := "rename|" + clientName + "\n"
		_, err := client.conn.Write([]byte(sendMsg))
		if err != nil {
			fmt.Println("设置用户名失败:", err)
			return
		}
		fmt.Println("已设置用户名:", clientName)
	}

	fmt.Println("连接服务器成功")
	client.Run()
}
