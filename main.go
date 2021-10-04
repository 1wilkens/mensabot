package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/mattermost/mattermost-server/v5/model"
	"github.com/yhat/scrape"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

const (
	VERSION = "v0.4"

	CANTEEN_URL_TODAY    = "http://speiseplan.studierendenwerk-hamburg.de/de/580/2018/0/"
	CANTEEN_URL_TOMORROW = "http://speiseplan.studierendenwerk-hamburg.de/de/580/2018/99/"
)

var REG_EXP_STATUS = regexp.MustCompile(`(?i)(?:^|\W)(alive|running|up)(?:$|\W)`)
var REG_EXP_HELP = regexp.MustCompile(`(?i)(?:^|\W)(command(|s)|help)(?:$|\W)`)
var REG_EXP_LEGEND = regexp.MustCompile(`(?i)(?:^|\W)(legend(|e)|zusatzstoff(|e)|nummer(|n))(?:$|\W)`)

var REG_EXP_TODAY = regexp.MustCompile(`(?i)(?:^|\W)(heute|today|hunger)(?:$|\W)`)
var REG_EXP_TOMORROW = regexp.MustCompile(`(?i)(?:^|\W)(morgen|tomorrow)(?:$|\W)`)

var REG_EXP_ORDER = regexp.MustCompile(`^@\w+ order (?P<command>open|submit|list|close) ?(?P<content>.*)$`)

//

var REG_EXP_THANKS = regexp.MustCompile(`(?i)(?:^|\W)(dank(|e)|thank(|s))(?:$|\W)`)

type config struct {
	MattermostApiURL string
	MattermostWsURL  string
	AuthToken        string

	TeamName    string
	DisplayName string

	ChannelNameDebug      string
	ChannelNameProduction string

	Favorites []string
}

var CONFIG config

type dish struct {
	name            string
	prices          [3]string
	isVegetarian    bool
	isVegan         bool
	containsBeef    bool
	containsPork    bool
	containsFish    bool
	containsChicken bool
	lactoseFree     bool
}

type mensabot struct {
	client   *model.Client4
	wsClient *model.WebSocketClient

	user *model.User
	team *model.Team

	channelDebug      *model.Channel

	orderUser   string
	orderDetail string
	orders      map[string]string
}

func (d dish) isFavorite() bool {
	name := strings.ToLower(d.name)
	for _, f := range CONFIG.Favorites {
		if strings.Contains(name, f) {
			return true
		}
	}
	return false
}

func (d dish) String() string {
	var buf bytes.Buffer
	buf.WriteString("| " + d.name + " |")
	if d.isFavorite() {
		buf.WriteString(" :heart_eyes:")
	}
	if d.isVegan {
		buf.WriteString(" :sunflower:")
	} else if d.isVegetarian {
		buf.WriteString(" :carrot:")
	}
	if d.containsBeef {
		buf.WriteString(" :cow2:")
	}
	if d.containsPork {
		buf.WriteString(" :pig2:")
	}
	if d.containsFish {
		buf.WriteString(" :fish:")
	}
	if d.containsChicken {
		buf.WriteString(" :rooster:")
	}
	if d.lactoseFree {
		buf.WriteString(" :milk_glass:")
	}
	buf.WriteString(" |")
	buf.WriteString(fmt.Sprintf(" %s // %s // %s |", d.prices[0], d.prices[1], d.prices[2]))
	return buf.String()
}

func trimNodeName(name string) (trimmed string) {
	trimmed = strings.Trim(name, " \t\n")
	trimmed = strings.Replace(trimmed, "( ", "(", -1)
	trimmed = strings.Replace(trimmed, " )", ")", -1)
	trimmed = strings.Replace(trimmed, " ,", ",", -1)
	trimmed = strings.Replace(trimmed, "  ", " ", -1)

	return
}

func dishFromNode(node *html.Node) dish {
	name := trimNodeName(scrape.Text(node))

	var prices [3]string
	var isVegetarian bool
	var isVegan bool
	var containsBeef bool
	var containsPork bool
	var containsFish bool
	var containsChicken bool
	var lactoseFree bool

	priceNodes := scrape.FindAll(node.Parent, scrape.ByClass("price"))
	imgNodes := scrape.FindAll(node, scrape.ByTag(atom.Img))

	for i, price := range priceNodes {
		prices[i] = strings.Replace(scrape.Text(price), "\xc2\xa0", "", -1)
	}

	for _, img := range imgNodes {
		switch strings.ToLower(scrape.Attr(img, "title")) {
		case "vegetarisch":
			isVegetarian = true
		case "vegan":
			isVegan = true
		case "mit rind":
			containsBeef = true
		case "mit schwein":
			containsPork = true
		case "mit fisch":
			containsFish = true
		case "mit geflügel":
			containsChicken = true
		case "laktosefrei":
			lactoseFree = true
		}
	}

	return dish{name, prices, isVegetarian || isVegan, isVegan, containsBeef, containsPork, containsFish, containsChicken, lactoseFree}
}

func getCanteenPlan(url string) (dishes []dish) {
	resp, err := http.Get(url)
	if err != nil {
		panic(err)
	}
	root, err := html.Parse(resp.Body)
	if err != nil {
		panic(err)
	}

	dishNodes := scrape.FindAll(root, scrape.ByClass("dish-description"))

	for _, dn := range dishNodes {
		dishes = append(dishes, dishFromNode(dn))
	}

	return
}

func newMensaBotFromConfig(cfg *config) (bot *mensabot) {
	println("[newMensaBotFromConfig] Connecting to " + cfg.MattermostApiURL)
	client := model.NewAPIv4Client(cfg.MattermostApiURL)

	bot = &mensabot{client: client}

	bot.setupGracefulShutdown()
	bot.ensureServerIsRunning()
	bot.loginAsBotUser(cfg.AuthToken)
	bot.setTeam(cfg.TeamName)

	if wsClient, err := model.NewWebSocketClient4(cfg.MattermostWsURL, bot.client.AuthToken); err != nil {
		println("[newMensaBotFromConfig] Failed to connect to the web socket")
		printError(err)
		panic(err)
	} else {
		bot.wsClient = wsClient
	}

	bot.channelDebug = bot.getChannel(cfg.ChannelNameDebug)

	return
}

func (bot *mensabot) setupGracefulShutdown() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	go func() {
		for _ = range c {
			if bot.wsClient != nil {
				bot.wsClient.Close()
			}

			bot.sendMessage("_["+CONFIG.DisplayName+"] has **stopped** running_", bot.channelDebug.Id, "")
			os.Exit(0)
		}
	}()
}

func (bot *mensabot) ensureServerIsRunning() {
	if props, resp := bot.client.GetOldClientConfig(""); resp.Error != nil {
		println("There was a problem pinging the Mattermost server.  Are you sure it's running?")
		printError(resp.Error)
		os.Exit(1)
	} else {
		println("[bot::ensureServerIsRunning] Server detected and is running version " + props["Version"])
	}
}

func (bot *mensabot) loginAsBotUser(token string) {
	bot.client.SetToken(token)
	if user, resp := bot.client.GetMe(""); resp.Error != nil {
		println("There was a problem logging into the Mattermost server.")
		printError(resp.Error)
		panic(resp.Error)
	} else {
		println("[bot::loginAsBotUser] Logged in as user '" + user.GetFullName() + "': " + user.Id)
		bot.user = user
	}
}

func (bot *mensabot) setTeam(teamName string) {
	if team, resp := bot.client.GetTeamByName(teamName, ""); resp.Error != nil {
		println("We failed to get the initial load")
		println("or we do not appear to be a member of the team '" + teamName + "'")
		printError(resp.Error)
		panic(resp.Error)
	} else {
		println("[bot::setTeam] Got team with name '" + teamName + "`: " + team.Id)
		bot.team = team
	}
}

func (bot *mensabot) getChannel(channelName string) *model.Channel {
	rChan, resp := bot.client.GetChannelByName(channelName, bot.team.Id, "")
	if resp.Error != nil {
		println("We failed to get the channel: " + channelName)
		printError(resp.Error)
		panic(resp.Error)
	}
	println("[bot::getChannel] Got channel with name '" + channelName + "': " + rChan.Id)
	return rChan
}

func (bot *mensabot) sendMessage(msg string, channelID string, replyToID string) {
	post := &model.Post{}
	post.ChannelId = channelID
	post.Message = msg
	post.RootId = replyToID

	if _, resp := bot.client.CreatePost(post); resp.Error != nil {
		println("We failed to send a message to channel: " + channelID)
		printError(resp.Error)
	}
}

func (bot *mensabot) startListening() {
	bot.sendMessage("_["+CONFIG.DisplayName+"] has **started** running_", bot.channelDebug.Id, "")
	bot.wsClient.Listen()

	for {
		select {
		case event := <-bot.wsClient.EventChannel:
			bot.handleWebSocketEvent(event)
		}
	}
}

func (bot *mensabot) handleWebSocketEvent(event *model.WebSocketEvent) {
	// Skip empty events to avoid noise (especially at shutdown)
	if event == nil {
		return
	}

	// This is for debugging purposes
	//fmt.Printf("[bot::handleWebSocketEvent] Handling event: %+v\n", event)

	// We only care about new posts
	if event.Event != model.WEBSOCKET_EVENT_POSTED {
		return
	}

	post := model.PostFromJson(strings.NewReader(event.Data["post"].(string)))
	if post != nil {
		// ignore my own posts
		if post.UserId == bot.user.Id {
			return
		}

		mention, ok := event.Data["mentions"].(string)
		if ok {
			// We have some mentions, check if we are one of them
			var mentions []string
			json.Unmarshal([]byte(mention), &mentions)
			if len(mentions) > 3 {
				// More than 3 mentions is probably @all or @channel, skip those
				return
			}
			for _, m := range mentions {
				if m == bot.user.Id {
					bot.handleCommand(post)
					return
				}
			}
		} else if event.Broadcast.ChannelId == bot.channelDebug.Id {
			bot.handleCommand(post)
		}
	}
}

func (bot *mensabot) writeDishes(dishes []dish, prefix string, channelID string, replyToID string) {
	var buf bytes.Buffer

	buf.WriteString(prefix + "\n\n")
	buf.WriteString("| Essen | Features | Preise |\n")
	buf.WriteString("| -- | -- | -- |\n")
	for _, d := range dishes {
		buf.WriteString(d.String() + "\n")
	}

	bot.sendMessage(buf.String(), channelID, replyToID)
}

func (bot *mensabot) handleOrder(post *model.Post) {

	var cmd string
	var content string

	groupNames := REG_EXP_ORDER.SubexpNames()
	for _, match := range REG_EXP_ORDER.FindAllStringSubmatch(post.Message, -1) {
		for idx, matchText := range match {
			name := groupNames[idx]
			if name == "command" {
				cmd = matchText
			} else if name == "content" {
				content = matchText
			}
		}
	}

	switch cmd {
	case "open":
		if bot.orderDetail != "" {
			if bot.orderUser == post.UserId {
				bot.orderDetail = content
				bot.sendMessage("Updated order details", post.ChannelId, post.Id)
				break
			}
			bot.sendMessage("Not overwriting active order", post.ChannelId, post.Id)
			break
		}

		user, _ := bot.client.GetUser(post.UserId, "")

		bot.orderUser = post.UserId
		bot.orderDetail = content
		bot.orders = make(map[string]string)

		msg := "#FoodOrder opened by @" + user.Username + ": " + bot.orderDetail
		bot.sendMessage(msg, post.ChannelId, post.Id)
		break
	case "submit":
		if bot.orderDetail == "" {
			bot.sendMessage("Cannot submit without active order", post.ChannelId, post.Id)
			break
		}
		bot.orders[post.UserId] = strings.Replace(content, "|", "", -1)
		break
	case "list":
		if bot.orderDetail == "" {
			bot.sendMessage("Cannot list without active order", post.ChannelId, post.Id)
			break
		}
		msg := "**[Active order]** " + bot.orderDetail + "\n\n"
		msg += "| User | Order |\n"
		msg += "| -- | -- |\n"
		for userId, order := range bot.orders {
			user, _ := bot.client.GetUser(userId, "")
			msg += "| @" + user.Username + " | " + order + " |\n"
		}
		bot.sendMessage(msg, post.ChannelId, post.Id)
		break
	case "close":
		if bot.orderDetail != "" && bot.orderUser != post.UserId {
			user, _ := bot.client.GetUser(bot.orderUser, "")
			msg := "Only @" + user.Username + " can close the active order"
			bot.sendMessage(msg, post.ChannelId, post.Id)
			break
		}
		if bot.orderDetail != "" {
			msg := "**Closing** active order:\n\n"
			msg += "| User | Order |\n"
			msg += "| -- | -- |\n"
			for userId, order := range bot.orders {
				user, _ := bot.client.GetUser(userId, "")
				msg += "| @" + user.Username + " | " + order + " |\n"
			}
			bot.orderDetail = ""
			bot.orderUser = ""
			bot.sendMessage(msg, post.ChannelId, post.Id)
		}
		break
	}

}

func (bot *mensabot) writeLegend(channelID string, replyToID string) {
	msg := "**Legende:**\n" +
		":heart_eyes: = Lieblingsgericht\n" +
		":sunflower: = Veganes Gericht\n" +
		":carrot: = Vegetarisches Gericht\n" +
		":cow2: = Enthält Rindfleisch\n" +
		":pig2: = Enthält Schweinefleisch\n" +
		":fish: = Enthält Fisch\n" +
		":rooster: = Enthält Geflügel\n" +
		":milk_glass: = Laktose**freies**(!) Gericht\n\n" +
		"**Zusatzstoffe:**\n" +
		"1 = Farbstoffe\n" +
		"2 = Konservierungsstoffe\n" +
		"3 = Antioxidationsmittel\n" +
		"4 = Geschmacksverstärker\n" +
		"5 = Geschwefelt\n" +
		"6 = Geschwärzt\n" +
		"7 = Gewachst\n" +
		"8 = Phosphat\n" +
		"9 = Süßungsmittel\n" +
		"10 = Phenylalaninquelle\n" +
		"14 = enthält glutenhaltiges Getreide (z. B. Weizen, Roggen, Gerste etc.)\n" +
		"15 = Krebstiere und Krebstiererzeugnisse\n" +
		"16 = Ei und Eierzeugnisse\n" +
		"17 = Fisch und Fischerzeugnisse\n" +
		"18 = Erdnüsse und Erdnusserzeugnisse\n" +
		"19 = Soja und Sojaerzeugnisse\n" +
		"20 = Milch und Milcherzeugnisse (einschl. Laktose)\n" +
		"21 = Schalenfrüchte (z.B. Mandel, Haselnüsse, Walnuss etc.)\n" +
		"22 = Sellerie und Sellerieerzeugnisse\n" +
		"23 = Senf und Senferzeugnisse\n" +
		"24 = Sesamsamen und Sesamsamenerzeugnisse\n" +
		"25 = Schwefeldioxid und Sulfite (Konzentration über 10mg/kg oder 10mg/l)\n" +
		"26 = Lupine und - erzeugnisse\n" +
		"27 = Mollusken/Weichtiere (z.B. Muscheln und Weinbergschnecken)\n"

	bot.sendMessage(msg, channelID, replyToID)
}

func (bot *mensabot) writeHelp(channelID string, replyToID string) {
	msg := "**Need help?** These are my supported commands:\n\n" +
		"| Command | Keyword(s) (completely case insensitive)|\n" +
		"| -- | -- |\n" +
		"| Status | alive, running, up |\n" +
		"| Today's canteen plan | heute, today, hunger |\n" +
		"| Tomorrow's canteen plan | morgen, tomorrow |\n" +
		"| Order controls | order [open, submit, list, close] |\n" +
		"| Legend | legend(e), zusatzstoff(e), nummer(n) |\n" +
		"| This help message | command(s), help |\n"

	bot.sendMessage(msg, channelID, replyToID)
}

func (bot *mensabot) writeMyPleasure(channelID string, replyToID string) {
	var msgs = [...]string{"My pleasure", "You are very welcome", "Dafür nicht", "Immer gern"}

	idx := rand.Intn(len(msgs))

	bot.sendMessage(msgs[idx], channelID, replyToID)
}

func (bot *mensabot) handleCommand(post *model.Post) {
	if REG_EXP_STATUS.MatchString(post.Message) {
		// If you see any word matching 'alive'/'running'/'up' then respond with status
		bot.sendMessage("Yes I'm up and running!", post.ChannelId, post.Id)
		return
	} else if REG_EXP_TODAY.MatchString(post.Message) {
		// If you see any word matching 'heute', 'today' or 'hunger', post today's canteen plan
		dishes := getCanteenPlan(CANTEEN_URL_TODAY)
		bot.writeDishes(dishes, "**Heute gibt es:**", post.ChannelId, post.Id)
	} else if REG_EXP_TOMORROW.MatchString(post.Message) {
		// If you see any word matching 'morgen' or 'tomorrow', post tomorrow's canteen plan
		dishes := getCanteenPlan(CANTEEN_URL_TOMORROW)
		bot.writeDishes(dishes, "**Morgen gibt es:**", post.ChannelId, post.Id)
	} else if REG_EXP_ORDER.MatchString(post.Message) {
		bot.handleOrder(post)
	} else if REG_EXP_LEGEND.MatchString(post.Message) {
		// If you see any word matching 'legend(e)', 'zusatzstoff(e)', 'inhaltsstoff(e)' or 'nummer(n)', post legend
		bot.writeLegend(post.ChannelId, post.Id)
	} else if REG_EXP_HELP.MatchString(post.Message) {
		// If you see any word matching 'command' or 'help', post available commands
		bot.writeHelp(post.ChannelId, post.Id)
	} else if REG_EXP_THANKS.MatchString(post.Message) {
		bot.writeMyPleasure(post.ChannelId, post.Id)
	} else {
		// If nothing matched post a generic message
		bot.sendMessage("**What does this even mean?!** (Type 'help' to get a list of available commands)", post.ChannelId, post.Id)
	}
}

func initialize() {
	if len(os.Args) < 2 {
		println("ERROR: MensaBot expects the configuration file as first argument!")
		os.Exit(1)
	}

	// Parse config
	cfgFile := os.Args[1]
	_, err := os.Stat(cfgFile)
	if err != nil {
		println("Config file is missing: " + cfgFile)
		panic(err)
	}
	if _, err := toml.DecodeFile(cfgFile, &CONFIG); err != nil {
		panic(err)
	}

	// Initialize rand
	rand.Seed(time.Now().Unix())
}

func main() {
	initialize()

	bot := newMensaBotFromConfig(&CONFIG)
	go bot.startListening()

	// Forever block main routine
	// TODO |2018-01-17|: It works without this, investigate what the best practices are
	select {}
}

func printError(err *model.AppError) {
	println("\tError Details:")
	println("\t\t" + err.Message)
	println("\t\t" + err.Id)
	println("\t\t" + err.DetailedError)
}
