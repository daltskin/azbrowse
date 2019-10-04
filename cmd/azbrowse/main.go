package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"github.com/lawrencegripper/azbrowse/internal/pkg/eventing"
	"github.com/lawrencegripper/azbrowse/internal/pkg/handlers"
	"github.com/lawrencegripper/azbrowse/internal/pkg/keybindings"
	"github.com/lawrencegripper/azbrowse/internal/pkg/search"
	"github.com/lawrencegripper/azbrowse/internal/pkg/tracing"
	"github.com/lawrencegripper/azbrowse/internal/pkg/views"
	"github.com/lawrencegripper/azbrowse/pkg/armclient"

	opentracing "github.com/opentracing/opentracing-go"
	"github.com/stuartleeks/gocui"
)

// Overridden via ldflags
var (
	version   = "99.0.1-devbuild"
	commit    = "unknown"
	date      = "unknown"
	goversion = "unknown"
)

var enableTracing bool
var hideGuids bool

func main() {
	var navigateToID string

	args := os.Args[1:] // skip first arg

	for len(args) >= 1 {
		arg := args[0]
		handled := false

		if strings.Contains(arg, "version") {
			fmt.Println(version)
			fmt.Println(commit)
			fmt.Println(date)
			fmt.Println(goversion)
			fmt.Println(fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH))
			os.Exit(0)
		}

		if strings.Contains(arg, "search") {
			fmt.Print("Getting resources \n")
			subRequest, _, err := getSubscriptions(context.Background())
			if err != nil {
				panic(err)
			}
			err = search.CrawlResources(context.Background(), subRequest)
			if err != nil {
				panic(err)
			}
			fmt.Print("Build suggester \n")

			suggester, _ := search.NewSuggester()
			fmt.Print("Get suggestions \n")

			suggestions := suggester.Autocomplete(args[1])
			fmt.Printf("%v \n", suggestions)
			os.Exit(0)
		}

		if strings.Contains(arg, "debug") {
			enableTracing = true
			tracing.EnableDebug()
			handled = true
		}
		if strings.Contains(arg, "demo") {
			hideGuids = true
			handled = true
		}

		if strings.Contains(arg, "navigate") {
			if len(args) >= 2 {
				navigateToID = args[1] // capture the next arg
				args = args[1:]        // move past the the captured arg
				handled = true
			}
		}

		if !handled {
			// unhandled arg
			fmt.Println("Usage:")
			fmt.Println("\tazbrowse [debug] [demo] [navigate <id to navigate to>]")
			fmt.Println()
			os.Exit(-1)
		}

		args = args[1:] // move to the next arg
	}

	confirmAndSelfUpdate()
	var ctx context.Context
	var span opentracing.Span

	if enableTracing {
		startTraceDashboardForSpan := tracing.StartTracing()

		rootCtx := context.Background()
		span, ctx = tracing.StartSpanFromContext(rootCtx, "azbrowseStart")

		startTraceDashboardForSpan(span)

		defer func() {
			// recover from panic if one occurred and show user the trace URL for debugging.
			if r := recover(); r != nil {
				fmt.Printf("A crash occurred: %s", r)
				debug.PrintStack()
				fmt.Printf("To see the trace details for the session visit: %s. \n Visit https://github.com/lawrencegripper/azbrowse/issues to raise a bug. \n Press any key to exit when you are done. \n", startTraceDashboardForSpan(span))

				bufio.NewReader(os.Stdin).ReadString('\n') //nolint:golint,errcheck
			}
		}()
	} else {

		rootCtx := context.Background()
		span, ctx = tracing.StartSpanFromContext(rootCtx, "azbrowseStart")

		defer func() {
			// recover from panic if one occurred and explain to the user how to proceed
			if r := recover(); r != nil {
				fmt.Printf("A crash occurred: %s", r)
				debug.PrintStack()
				fmt.Printf("To capture a more detailed train run 'azbrowse --debug' and reproduce the issue. Visit https://github.com/lawrencegripper/azbrowse/issues to raise a bug.")
			}
		}()
	}

	g, err := gocui.NewGui(gocui.OutputNormal)
	if err != nil {
		log.Panicln(err)
	}
	defer g.Close()

	g.Highlight = true
	g.SelFgColor = gocui.ColorCyan

	maxX, maxY := g.Size()
	// Padding
	maxX = maxX - 2
	maxY = maxY - 2

	if maxX < 72 {
		panic("I can't run in a terminal less than 72 wide ... it's tooooo small!!!")
	}

	leftColumnWidth := 45

	status := views.NewStatusbarWidget(1, maxY-2, maxX, hideGuids, g)
	content := views.NewItemWidget(leftColumnWidth+2, 1, maxX-leftColumnWidth-1, maxY-4, hideGuids, "")
	list := views.NewListWidget(ctx, 1, 1, leftColumnWidth, maxY-4, []string{"Loading..."}, 0, content, status, enableTracing, "Subscriptions")
	notifications := views.NewNotificationWidget(maxX-45, 1, 45, hideGuids, g)

	g.SetManager(status, content, list, notifications)
	g.SetCurrentView("listWidget")

	var editModeEnabled bool
	var isFullscreen bool
	var showHelp bool

	// Global handlers
	// NOTE> Global handlers must be registered first to
	//       ensure double key registration is prevented
	keybindings.AddHandler(keybindings.NewFullscreenHandler(list, content, &isFullscreen))
	keybindings.AddHandler(keybindings.NewCopyHandler(content, status))
	keybindings.AddHandler(keybindings.NewHelpHandler(&showHelp))
	keybindings.AddHandler(keybindings.NewQuitHandler())
	keybindings.AddHandler(keybindings.NewConfirmDeleteHandler(notifications))
	keybindings.AddHandler(keybindings.NewClearPendingDeleteHandler(notifications))

	// List handlers
	keybindings.AddHandler(keybindings.NewListDownHandler(list))
	keybindings.AddHandler(keybindings.NewListUpHandler(list))
	keybindings.AddHandler(keybindings.NewListExpandHandler(list))
	keybindings.AddHandler(keybindings.NewListRefreshHandler(list))
	keybindings.AddHandler(keybindings.NewListBackHandler(list))
	keybindings.AddHandler(keybindings.NewListBackLegacyHandler(list))
	keybindings.AddHandler(keybindings.NewListActionsHandler(list, ctx))
	keybindings.AddHandler(keybindings.NewListRightHandler(list, &editModeEnabled))
	keybindings.AddHandler(keybindings.NewListEditHandler(list, &editModeEnabled))
	keybindings.AddHandler(keybindings.NewListOpenHandler(list, ctx))
	keybindings.AddHandler(keybindings.NewListDeleteHandler(list, notifications))
	keybindings.AddHandler(keybindings.NewListUpdateHandler(list, status, ctx, content, g))
	keybindings.AddHandler(keybindings.NewListPageDownHandler(list))
	keybindings.AddHandler(keybindings.NewListPageUpHandler(list))
	keybindings.AddHandler(keybindings.NewListEndHandler(list))
	keybindings.AddHandler(keybindings.NewListHomeHandler(list))
	keybindings.AddHandler(keybindings.NewItemViewPageDownHandler(content))
	keybindings.AddHandler(keybindings.NewItemViewPageUpHandler(content))

	// Item handlers
	keybindings.AddHandler(keybindings.NewItemBackHandler(list))
	keybindings.AddHandler(keybindings.NewItemLeftHandler(&editModeEnabled))

	if err = keybindings.Bind(g); err != nil { // apply late binding for keys
		g.Close()

		fmt.Println("\n \n" + err.Error())
		os.Exit(1)
	}

	// Update views with keybindings
	keyBindings := keybindings.GetKeyBindingsAsStrings()
	status.HelpKeyBinding = strings.Join(keyBindings["help"], ",")
	list.ActionKeyBinding = strings.Join(keyBindings["listactions"], ",")
	list.FullscreenKeyBinding = strings.Join(keyBindings["fullscreen"], ",")
	notifications.ConfirmDeleteKeyBinding = strings.Join(keyBindings["confirmdelete"], ",")
	notifications.ClearPendingDeletesKeyBinding = strings.Join(keyBindings["clearpendingdeletes"], ",")

	go func() {
		time.Sleep(time.Second * 1)

		status.Status("Fetching Subscriptions", true)
		subRequest, data, err := getSubscriptions(ctx)
		if err != nil {
			g.Close()
			log.Panicln(err)
		}

		g.Update(func(gui *gocui.Gui) error {
			g.SetCurrentView("listWidget")

			status.Status("Getting provider data", true)

			armclient.PopulateResourceAPILookup(ctx)
			status.Status("Done getting provider data", false)

			newList := []*handlers.TreeNode{}
			for _, sub := range subRequest.Subs {
				newList = append(newList, &handlers.TreeNode{
					Display:        sub.DisplayName,
					Name:           sub.DisplayName,
					ID:             sub.ID,
					ExpandURL:      sub.ID + "/resourceGroups?api-version=2018-05-01",
					ItemType:       handlers.SubscriptionType,
					SubscriptionID: sub.SubscriptionID,
				})
			}

			var newContent string
			var newTitle string
			if err != nil {
				newContent = err.Error()
				newTitle = "Error"
			} else {
				newContent = data
				newTitle = "Subscriptions response"
			}

			list.Navigate(newList, newContent, newTitle)

			return nil
		})

		status.Status("Fetching Subscriptions: Completed", false)

	}()

	if navigateToID != "" {
		go func() {
			navigatedChannel := eventing.SubscribeToTopic("list.navigated")
			var lastNavigatedNode *handlers.TreeNode

			processNavigations := true

			for {
				nodeListInterface := <-navigatedChannel

				if processNavigations {
					nodeList := nodeListInterface.([]*handlers.TreeNode)

					if lastNavigatedNode != nil && lastNavigatedNode != list.CurrentExpandedItem() {
						processNavigations = false
					} else {

						gotNode := false
						for nodeIndex, node := range nodeList {
							// use prefix matching
							// but need additional checks as target of /foo/bar would be matched by  /foo/bar  and /foo/ba
							// additional check is that the lengths match, or the next char in target is a '/'
							if strings.HasPrefix(navigateToID, node.ID) && (len(navigateToID) == len(node.ID) || navigateToID[len(node.ID)] == '/') {
								list.ChangeSelection(nodeIndex)
								lastNavigatedNode = node
								list.ExpandCurrentSelection()
								gotNode = true
								break
							}
						}

						if !gotNode {
							// we got as far as we could - now stop!
							processNavigations = false
						}
					}
				}
			}
		}()
	}

	span.Finish()

	if err := g.MainLoop(); err != nil && err != gocui.ErrQuit {
		log.Panicln(err)
	}

}

func getSubscriptions(ctx context.Context) (armclient.SubResponse, string, error) {
	span, ctx := tracing.StartSpanFromContext(ctx, "expand:subs")
	defer span.Finish()

	// Get Subscriptions
	data, err := armclient.DoRequest(ctx, "GET", "/subscriptions?api-version=2018-01-01")
	if err != nil {
		return armclient.SubResponse{}, "", fmt.Errorf("Failed to load subscriptions: %s", err)
	}

	var subRequest armclient.SubResponse
	err = json.Unmarshal([]byte(data), &subRequest)
	if err != nil {
		return armclient.SubResponse{}, "", fmt.Errorf("Failed to load subscriptions: %s", err)
	}
	return subRequest, data, nil
}
