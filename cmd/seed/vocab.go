package main

// Synthetic vocabulary. Store, product and customer names are generated from
// these lists; none is a real brand or person.

type business struct {
	kind       string
	categories []category
}

type category struct {
	name   string
	nouns  []string
	price  float64 // median price in rupees
	spread float64 // lognormal sigma
}

var businesses = []business{
	{"fashion", []category{
		{"Kurtas", []string{"kurta", "kurti", "tunic"}, 899, 0.5}, {"Sarees", []string{"saree", "drape"}, 1999, 0.7},
		{"Shirts", []string{"shirt", "overshirt"}, 799, 0.4}, {"Denim", []string{"jeans", "denim jacket"}, 1499, 0.4},
		{"Footwear", []string{"sandals", "sneakers", "juttis"}, 1299, 0.5}, {"Bags", []string{"tote", "sling bag", "backpack"}, 999, 0.6},
		{"Ethnic sets", []string{"kurta set", "lehenga"}, 2999, 0.6}, {"T-shirts", []string{"t-shirt", "polo"}, 499, 0.3},
		{"Dupattas", []string{"dupatta", "stole"}, 599, 0.4}, {"Jewellery", []string{"jhumkas", "bangles", "necklace"}, 699, 0.8},
		{"Winterwear", []string{"shawl", "cardigan", "jacket"}, 1799, 0.5}, {"Kidswear", []string{"frock", "romper", "kids kurta"}, 649, 0.4},
		{"Innerwear", []string{"vest", "socks pack"}, 349, 0.3}, {"Watches", []string{"analog watch", "strap watch"}, 1899, 0.6},
	}},
	{"home", []category{
		{"Cookware", []string{"kadhai", "tawa", "pressure cooker"}, 1299, 0.5}, {"Storage", []string{"steel dabba set", "jar set"}, 599, 0.4},
		{"Bedding", []string{"bedsheet", "quilt", "pillow cover set"}, 1199, 0.5}, {"Decor", []string{"brass diya", "wall hanging", "vase"}, 799, 0.6},
		{"Lighting", []string{"table lamp", "string lights"}, 999, 0.5}, {"Bath", []string{"towel set", "bath mat"}, 649, 0.4},
		{"Dinnerware", []string{"thali set", "dinner plates"}, 1499, 0.5}, {"Cleaning", []string{"mop", "broom set"}, 399, 0.3},
		{"Furniture", []string{"bookshelf", "stool", "side table"}, 3999, 0.7}, {"Curtains", []string{"curtain pair", "blinds"}, 1099, 0.4},
		{"Appliances", []string{"mixer grinder", "kettle", "iron"}, 2499, 0.6}, {"Garden", []string{"planter", "seed kit"}, 449, 0.5},
		{"Pooja items", []string{"pooja thali", "incense set"}, 349, 0.5},
	}},
	{"electronics", []category{
		{"Phone accessories", []string{"charger", "phone case", "cable"}, 499, 0.6}, {"Audio", []string{"earbuds", "speaker", "headphones"}, 1999, 0.8},
		{"Wearables", []string{"fitness band", "smartwatch"}, 2999, 0.6}, {"Computer accessories", []string{"mouse", "keyboard", "webcam"}, 999, 0.6},
		{"Storage devices", []string{"pen drive", "SSD", "memory card"}, 1199, 0.7}, {"Smart home", []string{"smart plug", "smart bulb"}, 899, 0.5},
		{"Cameras", []string{"action camera", "tripod"}, 4999, 0.8}, {"Networking", []string{"router", "range extender"}, 1799, 0.5},
		{"Power", []string{"power bank", "surge protector"}, 1299, 0.4}, {"Gaming", []string{"controller", "gaming mouse"}, 2499, 0.6},
		{"Cables", []string{"HDMI cable", "USB hub"}, 399, 0.4}, {"Tablets", []string{"tablet stand", "stylus"}, 799, 0.5},
	}},
	{"grocery", []category{
		{"Staples", []string{"atta", "basmati rice", "toor dal"}, 349, 0.4}, {"Spices", []string{"garam masala", "turmeric", "chilli powder"}, 149, 0.5},
		{"Snacks", []string{"namkeen", "khakhra", "chikki"}, 129, 0.4}, {"Beverages", []string{"masala chai", "filter coffee"}, 299, 0.5},
		{"Dairy", []string{"ghee", "paneer"}, 399, 0.4}, {"Oils", []string{"mustard oil", "groundnut oil"}, 249, 0.3},
		{"Sweets", []string{"soan papdi", "ladoo box"}, 399, 0.5}, {"Dry fruits", []string{"cashews", "almonds", "raisins"}, 699, 0.5},
		{"Breakfast", []string{"poha", "oats", "muesli"}, 199, 0.4}, {"Pickles", []string{"mango pickle", "lime pickle"}, 179, 0.3},
		{"Organic", []string{"jaggery", "millet flour"}, 249, 0.4}, {"Household", []string{"detergent", "dishwash bar"}, 199, 0.4},
	}},
	{"beauty", []category{
		{"Skincare", []string{"face wash", "moisturiser", "sunscreen"}, 449, 0.5}, {"Haircare", []string{"hair oil", "shampoo"}, 349, 0.4},
		{"Makeup", []string{"kajal", "lipstick", "compact"}, 399, 0.6}, {"Fragrance", []string{"attar", "body mist"}, 699, 0.6},
		{"Ayurveda", []string{"ubtan", "herbal soap"}, 249, 0.4}, {"Men's grooming", []string{"beard oil", "trimmer"}, 899, 0.7},
		{"Bath & body", []string{"body lotion", "scrub"}, 399, 0.4}, {"Nails", []string{"nail paint", "nail kit"}, 199, 0.4},
		{"Tools", []string{"hair dryer", "straightener"}, 1499, 0.5}, {"Gift sets", []string{"gift hamper", "travel kit"}, 999, 0.5},
		{"Oral care", []string{"herbal toothpaste", "toothbrush pack"}, 149, 0.3}, {"Wellness", []string{"protein powder", "multivitamin"}, 1199, 0.5},
	}},
	{"books", []category{
		{"Fiction", []string{"novel", "short stories"}, 399, 0.4}, {"Exam prep", []string{"practice guide", "question bank"}, 549, 0.4},
		{"Children", []string{"picture book", "activity book"}, 249, 0.4}, {"Regional", []string{"Hindi novel", "Marathi poetry"}, 299, 0.4},
		{"Business", []string{"management book", "biography"}, 499, 0.4}, {"Stationery", []string{"notebook set", "pen set", "planner"}, 299, 0.5},
		{"Comics", []string{"graphic novel", "comic box"}, 349, 0.5}, {"Self help", []string{"habits guide", "journal"}, 399, 0.4},
		{"Art supplies", []string{"watercolour set", "sketchbook"}, 599, 0.5}, {"Academic", []string{"textbook", "reference book"}, 699, 0.4},
		{"Spiritual", []string{"Gita commentary", "meditation guide"}, 349, 0.4}, {"Cookbooks", []string{"regional cookbook"}, 499, 0.3},
	}},
}

var adjectives = []string{"Cotton", "Classic", "Handwoven", "Everyday", "Premium", "Compact", "Organic", "Festive", "Lightweight",
	"Heritage", "Urban", "Essential", "Deluxe", "Eco", "Royal", "Studio", "Travel", "Signature", "Pure", "Modern"}

var colours = []string{"indigo", "mustard", "teal", "maroon", "ivory", "olive", "charcoal", "saffron", "rose", "sky blue", "black", "white"}

var storeWords = []string{"Saanvi", "Kaveri", "Nilgiri", "Tarang", "Vasant", "Meghna", "Aranya", "Chandan", "Sitara", "Prakriti",
	"Ujjwal", "Barkha", "Kesar", "Mayur", "Shilpa", "Gulmohar", "Sagar", "Anokhi", "Vihaan", "Jharna"}

type city struct{ name, region string }

var cities = []city{
	{"Mumbai", "West"}, {"Delhi", "North"}, {"Bengaluru", "South"}, {"Hyderabad", "South"}, {"Ahmedabad", "West"}, {"Chennai", "South"},
	{"Kolkata", "East"}, {"Surat", "West"}, {"Pune", "West"}, {"Jaipur", "North"}, {"Lucknow", "North"}, {"Kanpur", "North"},
	{"Nagpur", "West"}, {"Indore", "Central"}, {"Thane", "West"}, {"Bhopal", "Central"}, {"Visakhapatnam", "South"}, {"Patna", "East"},
	{"Vadodara", "West"}, {"Ghaziabad", "North"}, {"Ludhiana", "North"}, {"Agra", "North"}, {"Nashik", "West"}, {"Faridabad", "North"},
	{"Meerut", "North"}, {"Rajkot", "West"}, {"Varanasi", "North"}, {"Srinagar", "North"}, {"Aurangabad", "West"}, {"Dhanbad", "East"},
	{"Amritsar", "North"}, {"Allahabad", "North"}, {"Ranchi", "East"}, {"Howrah", "East"}, {"Coimbatore", "South"}, {"Jabalpur", "Central"},
	{"Gwalior", "Central"}, {"Vijayawada", "South"}, {"Jodhpur", "North"}, {"Madurai", "South"}, {"Raipur", "Central"}, {"Kota", "North"},
	{"Guwahati", "Northeast"}, {"Chandigarh", "North"}, {"Solapur", "West"}, {"Hubballi", "South"}, {"Tiruchirappalli", "South"},
	{"Bareilly", "North"}, {"Mysuru", "South"}, {"Tiruppur", "South"}, {"Gurugram", "North"}, {"Aligarh", "North"}, {"Jalandhar", "North"},
	{"Bhubaneswar", "East"}, {"Salem", "South"}, {"Warangal", "South"}, {"Thiruvananthapuram", "South"}, {"Bhiwandi", "West"},
	{"Saharanpur", "North"}, {"Gorakhpur", "North"}, {"Guntur", "South"}, {"Bikaner", "North"}, {"Amravati", "West"}, {"Noida", "North"},
	{"Jamshedpur", "East"}, {"Bhilai", "Central"}, {"Cuttack", "East"}, {"Firozabad", "North"}, {"Kochi", "South"}, {"Nellore", "South"},
	{"Bhavnagar", "West"}, {"Dehradun", "North"}, {"Durgapur", "East"}, {"Asansol", "East"}, {"Rourkela", "East"}, {"Nanded", "West"},
	{"Kolhapur", "West"}, {"Ajmer", "North"}, {"Gulbarga", "South"}, {"Jamnagar", "West"}, {"Ujjain", "Central"}, {"Siliguri", "East"},
	{"Jhansi", "North"}, {"Jammu", "North"}, {"Mangaluru", "South"}, {"Erode", "South"}, {"Belagavi", "South"}, {"Tirunelveli", "South"},
	{"Gaya", "East"}, {"Udaipur", "North"}, {"Davanagere", "South"}, {"Kozhikode", "South"}, {"Akola", "West"}, {"Kurnool", "South"},
	{"Bokaro", "East"}, {"Bellary", "South"}, {"Patiala", "North"}, {"Agartala", "Northeast"}, {"Bhagalpur", "East"}, {"Latur", "West"},
	{"Dhule", "West"}, {"Korba", "Central"}, {"Bhilwara", "North"}, {"Brahmapur", "East"}, {"Muzaffarpur", "East"}, {"Ahmednagar", "West"},
	{"Mathura", "North"}, {"Kollam", "South"}, {"Bilaspur", "Central"}, {"Shahjahanpur", "North"}, {"Thrissur", "South"}, {"Alwar", "North"},
	{"Kakinada", "South"}, {"Nizamabad", "South"}, {"Sagar", "Central"}, {"Tumakuru", "South"}, {"Hisar", "North"}, {"Rohtak", "North"},
	{"Panipat", "North"}, {"Darbhanga", "East"}, {"Kharagpur", "East"}, {"Aizawl", "Northeast"}, {"Ichalkaranji", "West"}, {"Tirupati", "South"},
	{"Karnal", "North"}, {"Bathinda", "North"}, {"Rampur", "North"}, {"Shivamogga", "South"}, {"Ratlam", "Central"}, {"Modinagar", "North"},
	{"Durg", "Central"}, {"Shillong", "Northeast"}, {"Imphal", "Northeast"}, {"Hapur", "North"}, {"Anantapur", "South"}, {"Arrah", "East"},
	{"Karimnagar", "South"}, {"Etawah", "North"}, {"Bharatpur", "North"}, {"Begusarai", "East"}, {"Gandhidham", "West"}, {"Puducherry", "South"},
	{"Sikar", "North"}, {"Thoothukudi", "South"}, {"Rewa", "Central"}, {"Mirzapur", "North"}, {"Raichur", "South"}, {"Pali", "North"},
	{"Haridwar", "North"}, {"Vellore", "South"}, {"Panaji", "West"}, {"Gangtok", "Northeast"},
}

var payments = []struct {
	name   string
	weight float64
}{{"upi", 0.55}, {"card", 0.20}, {"cod", 0.14}, {"netbanking", 0.07}, {"wallet", 0.04}}

var channels = []struct {
	name   string
	weight float64
}{{"app", 0.52}, {"web", 0.38}, {"marketplace", 0.10}}

// SOC alert vocabulary (W9): Mitigata's own shape of data.
var socSources = []string{"EDR", "Firewall", "Email gateway", "SIEM correlation", "Cloud posture", "Identity provider", "WAF", "DLP"}
var socTactics = []string{"Reconnaissance", "Resource Development", "Initial Access", "Execution", "Persistence", "Privilege Escalation",
	"Defense Evasion", "Credential Access", "Discovery", "Lateral Movement", "Collection", "Command and Control", "Exfiltration", "Impact"}
var socSeverity = []struct {
	name   string
	weight float64
}{{"critical", 0.02}, {"high", 0.10}, {"medium", 0.33}, {"low", 0.40}, {"info", 0.15}}
