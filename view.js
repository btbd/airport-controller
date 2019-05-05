var elEvent = document.getElementById("event");
var elFids = document.getElementById("fids");

var sprite = {
    truck: document.getElementById("truck"),
    person: document.getElementById("person"),
    shop: document.getElementById("shop"),
    cup: document.getElementById("cup"),
    warehouse: document.getElementById("warehouse"),
    tower: document.getElementById("tower")
};

function Supplier(logo) {
    this.width = 0;
    this.height = 0;
    this.x = 0;
    this.y = 0;
    this.logo = new Image();
    this.logo.src = logo;
}

function Carrier(logo) {
    this.start = 0;
    this.supplier = undefined;
    this.retailer = undefined;
    this.width = 0;
    this.height = 0;
    this.x = 0;
    this.y = 0;
    this.logo = new Image();
    this.logo.src = logo;
}

function Retailer(logo) {
    this.width = 0;
    this.height = 0;
    this.x = 0;
    this.y = 0;
    this.customers = [];
    this.small = 10;
    this.medium = 10;
    this.large = 10;
    this.logo = new Image();
    this.logo.src = logo;
}

function Customer(retailer) {
    this.time = 0;
    this.start = Date.now();
    this.retailer = retailer;
    this.width = 0;
    this.height = 0;
    this.x = retailer.x != 0 ? retailer.x : canvas.width / 2;
    this.y = canvas.height + 25;
    this.sx = this.x;
    this.sy = this.y;
    this.speed = 2;
    this.satisfied = false;
    this.z = 0;
    this.vz = 0;

    retailer.customers.push(this);
}

var canvas = document.getElementById("canvas"),
    ctx = canvas.getContext("2d", { alpha: false });

document.getElementById("fids-holder").onclick = canvas.onclick = function() { elEvent.classList.add("hide"); };
ctx.scale(2, 2);
var oldDrawImage = ctx.drawImage;
ctx.drawImage = function( ){
    try {
        oldDrawImage.apply(this, arguments);
    } catch (e) {}
};

var suppliers = [],
    carriers = [],
    retailers = [],
    customers = [];

(function connect() {
    suppliers.length = carriers.length = retailers.length = customers.length = 0;

    httpGet("./data", function(x) {
        if (x.readyState === 4) {
            if (x.status === 200) {
                var data = JSON.parse(x.responseText);
                if (data) {
                    if (data.suppliers) {
                        for (var i = 0; i < data.suppliers.length; ++i) {
                            suppliers.push(new Supplier(data.suppliers[i].logo));
                        }
                    }

                    if (data.retailers) {
                        for (var i = 0; i < data.retailers.length; ++i) {
                            var dr = data.retailers[i];
                            var r = new Retailer(dr.logo);
                            if (dr.customers) {
                                for (var e = 0; e < dr.customers.length; ++e) {
                                    new Customer(r);
                                }
                            }

                            retailers.push(r);
                        }
                    }

                    carriers.length = 0;
                    if (data.carriers) {
                        for (var i = 0; i < data.carriers.length; ++i) {
                            carriers.push(new Carrier(data.carriers[i].logo));
                        }
                    }

                    var ws = new WebSocket(((window.location.protocol === "https:") ? "wss://" : "ws://") + window.location.host + window.location.pathname.replace(/(view)(?!.*\/)/, "ws_view"));
                    ws.onmessage = function(e) {
                        var d = JSON.parse(e.data);
                        if (d) {
                            switch (d.type) {
                                case "event":
                                    var row = elFids.insertRow(1);
                                    var event = d.event;
                                    var time = new Date(event.time);
                                    var h = time.getHours();
                                    if (h < 10) h = "0" + h;
                                    var m = time.getMinutes();
                                    if (m < 10) m = "0" + m;
									var tt = event.type.split(".")[0];
									row.innerHTML = "<td>" + h + ":" + m + "</td><td>" + event.source.split(".")[0] + "</td><td>" + tt + "</td>";
                                    row.onclick = function(e) {
                                        elEvent.children[0].innerText = JSON.stringify(event, null, 4);
                                        elEvent.classList.remove("hide");
                                        e.stopPropagation();
                                    };
                                    for (var i = elFids.rows.length; i > 50; --i) elFids.deleteRow(50);
                                    break;
                                case "customer":
                                    new Customer(retailers[d.r]);
                                    break;
                                case "jump":
                                    var c = retailers[d.r].customers[d.c];
                                    if (c.z === 0) retailers[d.r].customers[d.c].vz = canvas.height * 0.0075;
                                    break;
                                case "satisfied":
                                    retailers[d.r].customers[d.c].satisfied = true;
                                    break;
                                case "retailer":
                                    retailers.push(new Retailer(d.logo));
                                    break;
                                case "rmretailer":
                                    var r = retailers.splice(d.r, 1)[0];
                                    for (var i = 0; i < r.customers.length; ++i) {
                                        var c = r.customers[i];
                                        c.speed = -0.5 + Math.random();
                                        c.start = Date.now();
                                        customers.push(c);
                                    }
                                    break;
                                case "supplier":
                                    suppliers.push(new Supplier(d.logo));
                                    break;
                                case "rmsupplier":
                                    for (var i = 0; i < carriers.length; ++i) {
                                        if (carriers[i].supplier == d.s) {
                                            carriers.splice(i--, 1);
                                        }
                                    }
                                    suppliers.splice(d.s, 1);
                                    break;
                                case "carrier":
                                    carriers.push(new Carrier(d.logo));
                                    break;
                                case "rmcarrier":
                                    carriers.splice(d.c, 1);
                                    break;
                                case "gocarrier":
                                    carriers[d.c].supplier = suppliers[d.s];
                                    carriers[d.c].retailer = retailers[d.r];
                                    carriers[d.c].start = Date.now();
                                    break;
                                case "small":
                                    retailers[d.r].small = d.c;
                                    break;
                                case "medium":
                                    retailers[d.r].medium = d.c;
                                    break;
                                case "large":
                                    retailers[d.r].large = d.c;
                                    break;
                            }
                        }
                    };

                    ws.onclose = connect;
                }
            } else {
                setTimeout(connect, 500);
            }
        }
    });
})();

function drawImage(img, x, y, width, height, angle) {
    ctx.translate(x, y);
    ctx.rotate(angle);
    ctx.drawImage(img, -width / 2, -height / 2, width, height);
    ctx.rotate(-angle);
    ctx.translate(-x, -y);
}

function drawLogo(img, x, y, r) {
    if (img && img.complete) {
        var w = r * 2;
        var h = (img.height / img.width) * w;
				
		ctx.fillStyle = "white";
		ctx.beginPath();
		ctx.arc(x, y, r, 0, 2 * Math.PI);
		ctx.closePath();
		ctx.fill();
				
		ctx.save();
		ctx.beginPath();
		ctx.arc(x, y, r, 0, 2 * Math.PI);
		ctx.closePath();
		ctx.clip();
		ctx.drawImage(img, x - (w / 2), y - (h / 2), w, h);
		ctx.beginPath();
		ctx.arc(x, y, r, 0, 2 * Math.PI);
		ctx.clip();
		ctx.closePath();
		ctx.restore();
    }
}

function drawSprite(img, x, y, width, height, framex, framesx, framey, framesy) {
    var w = img.width / framesx;
    var h = img.height / framesy;
    ctx.shadowColor = "black";
    ctx.shadowBlur = 5;
    ctx.shadowOffsetY = canvas.width * 0.002;
    ctx.drawImage(img, framex * w, framey * h, w, h, x, y, width, height);
    ctx.shadowBlur = ctx.shadowOffsetY = 0;
}

function drawBubble(hx, hy, x, y, w, h, radius) {
    var r = x + w,
        f = y - h;
    ctx.beginPath();
    ctx.lineWidth = w * 0.02;
    ctx.moveTo(x + radius, y);
    ctx.lineTo(hx, hy);
    ctx.lineTo(x + radius * 3, y);
    ctx.lineTo(r - radius, y);
    ctx.quadraticCurveTo(r, y, r, y - radius);
    ctx.lineTo(r, y - h + radius);
    ctx.quadraticCurveTo(r, f, r - radius, f);
    ctx.lineTo(x + radius, f);
    ctx.quadraticCurveTo(x, f, x, f + radius);
    ctx.lineTo(x, y - radius);
    ctx.quadraticCurveTo(x, y, x + radius, y);
    ctx.fill();
    ctx.stroke();
}

(function update() {
    var airport_y = (4 / 7) * canvas.height;

    ctx.fillStyle = "lightgreen";
    ctx.fillRect(0, 0, canvas.width, canvas.height);
    ctx.strokeStyle = "white";
    ctx.lineWidth = canvas.width * 0.005;
    for (var i = 0; i < carriers.length; ++i) {
        var c = carriers[i];
        c.width = c.height = canvas.width * 0.06;
        var px = canvas.width - c.width / 1.5;
        var py = canvas.height / 3 + (i * c.height) - ((carriers.length) * c.height / 2);
        var cx = px - c.width / 1.5;
        var cy = py - c.height / 2;
        ctx.fillStyle = "#3a3a39";
        ctx.fillRect(cx, cy, c.width * 2, c.height);
        ctx.beginPath();
        ctx.moveTo(cx, cy);
        ctx.lineTo(canvas.width, cy);
        ctx.stroke();
        ctx.beginPath();
        ctx.moveTo(cx, py + c.height / 2);
        ctx.lineTo(canvas.width, py + c.height / 2);
        ctx.stroke();

        if (c.supplier && c.retailer) {
            var t = (Date.now() - c.start) / 1000;
            var x = c.x;
            var y = c.y;
            var o = c.width / 10;

            ctx.save();
            ctx.rect(0, c.supplier.y + c.supplier.height, canvas.width, canvas.height);
            ctx.clip();
            if (t < 2) {
                t /= 2;
                c.x = c.supplier.x + (1 - t) * (px - c.supplier.x);
                c.y = py + Math.pow(t, 9) * (c.supplier.y - py);
                drawImage(sprite.truck, c.x, c.y, c.width, c.height, Math.atan2(y - c.y, x - c.x));
            } else if (t >= 2 && t < 4) {
                t -= 2;
                t /= 2;
                var dx = c.retailer.x - c.supplier.x;
                var dy = c.retailer.y - c.supplier.y + (c.height / 2);

                c.x = c.supplier.x + (Math.sin(Math.PI * t - Math.PI / 2) / 2 + 1 / 2) * dx;
                if (Math.abs(dx) < canvas.width / 8) {
                    c.y = c.supplier.y + c.supplier.height / 2 - c.height / 2 + (Math.sin(Math.PI * t - Math.PI / 2) / 2 + 1 / 2) * dy;
                } else {
                    c.y = c.supplier.y + c.supplier.height / 2 - c.height / 2 + (Math.pow(Math.sin(Math.PI * t - Math.PI / 2), 5) / 2 + 1 / 2) * dy;
                }
                drawImage(sprite.truck, c.x, c.y, c.width, c.height, Math.atan2(y - c.y, x - c.x));
            } else if (t < 6) {
                t -= 4;
                t /= 2;
                t = 1 - t;
                c.x = c.retailer.x + (1 - t) * (px - c.retailer.x);
                c.y = py + Math.pow(t, 5) * (c.retailer.y - py);
                drawImage(sprite.truck, c.x, c.y, c.width, c.height, Math.atan2(c.y - y, c.x - x));
                o = -o;
            } else {
                c.supplier = c.retailer = undefined;
                drawImage(sprite.truck, c.x = px, c.y = py, c.width, c.height, 0);
                o = 0;
            }

            var a = Math.atan2(y - c.y, x - c.x);
            drawLogo(c.logo, c.x + o * Math.cos(a), c.y + o * Math.sin(a), c.height * 0.2);
            ctx.restore();
        } else {
            drawImage(sprite.truck, c.x = px, c.y = py, c.width, c.height, 0);
            drawLogo(c.logo, c.x + c.width / 10, c.y, c.height * 0.2);
        }
    }

    for (var i = 0, o = canvas.width / (suppliers.length + 1), x = o; i < suppliers.length; ++i, x += o) {
        var s = suppliers[i];

        s.height = 0.10 * canvas.height;
        s.width = 1.9 * s.height;
        s.x = x;
        s.y = 0;

        ctx.fillStyle = "gray";
        ctx.drawImage(sprite.warehouse, s.x - s.width / 2, s.y, s.width, s.height);
        drawLogo(s.logo, s.x, s.y + s.height / 1.7, s.height * 0.3);
    }

    var w = canvas.height * 0.25;
    var h = w * sprite.tower.height / sprite.tower.width;
    ctx.drawImage(sprite.tower, canvas.width * 0.05 - w / 2, canvas.height / 2 - h / 1.25, w, h);

    ctx.beginPath();
    ctx.fillStyle = "#FEFE00";
    ctx.ellipse(canvas.width / 2, airport_y, canvas.width * 0.55, canvas.height * 0.075, 0, Math.PI / 2, true);
    ctx.closePath();
    ctx.fill();
    ctx.fillStyle = "black";
    ctx.font = "bold " + (canvas.height * 0.05) + "px Arial";
    ctx.textAlign = "center";
    ctx.fillText("HEATHROW", canvas.width / 2, airport_y - (canvas.height * 0.01875));

    ctx.fillStyle = "gray";
    ctx.fillRect(0, airport_y, canvas.width, canvas.height);

    for (var i = 0, o = canvas.width / (retailers.length + 1), x = o; i < retailers.length; ++i, x += o) {
        var r = retailers[i];

        r.width = r.height = canvas.height * 0.10;
        r.x = x;
        r.y = airport_y;

        if (r.small == 0) {
            ctx.strokeStyle = "black";
            ctx.fillStyle = "white";
            var s = r.width * 0.5;
            var cx = r.x - r.width;
            drawBubble(r.x - r.width / 2, r.y + r.height / 4, cx, r.y, s, s, s * 0.25);
            var cs = s * 0.5;
            ctx.drawImage(sprite.cup, cx + cs / 2, r.y - s * 0.8, cs, cs);
        }

        if (r.medium == 0) {
            ctx.strokeStyle = "black";
            ctx.fillStyle = "white";
            var s = r.width * 0.5;
            var cx = r.x - s / 2;
            drawBubble(r.x, r.y, cx, r.y - s / 2, s, s, s * 0.25);
            var cs = s * 0.75;
            ctx.drawImage(sprite.cup, cx + s * 0.15, r.y - s - cs / 1.5, cs, cs);
        }

        if (r.large == 0) {
            ctx.strokeStyle = "black";
            ctx.fillStyle = "white";
            var s = r.width * 0.5;
            var cx = r.x + r.width / 2;
            drawBubble(r.x + r.width / 2, r.y + r.height / 4, cx, r.y, s, s, s * 0.25);
            ctx.drawImage(sprite.cup, cx, r.y - s * 1.1, s, s);
        }

        ctx.drawImage(sprite.shop, r.x - r.width / 2, r.y, r.width, r.height);
        drawLogo(r.logo, r.x, r.y, r.height * 0.2);

        for (var e = 0; e < r.customers.length; ++e) {
            var c = r.customers[e];
            c.time += 0.3;
            c.width = c.height = canvas.height * 0.05;

            if (e == 0 && c.satisfied) {
                c.speed *= (Math.random() > 0.5 ? 1 : -1) * (0.75 + Math.random() * 0.5);
                c.start = Date.now();
                customers.push(c);
                r.customers.splice(e--, 1);
            } else {
                var tx = r.x;
                var ty = r.y + r.height + (e * c.height / 2);
                var dx = tx - c.x;
                var dy = ty - c.y;
                var d = Math.sqrt(dx * dx + dy * dy);
                var frame = 1 + (Math.floor(c.time) % 8);
                if (d > 1) {
                    if (d < canvas.height * 0.03) {
                        frame = 0;
                        var s = c.speed * (canvas.height / 1080) * (d / 25);

                        var a = Math.atan2(dy, dx + (-10 + 10 * Math.random()));
                        c.x += Math.cos(a) * s;
                        c.y += Math.sin(a) * s;
                    } else {
                        var t = (Date.now() - c.start) / 1000;
                        if (t >= 2) {
                            c.x = tx;
                            c.y = ty;
                        } else {
                            c.x = ((tx - c.sx) * t / 2) + c.sx;
                            c.y = ((ty - c.sy) * t / 2) + c.sy;
                        }
                    }
                } else {
                    frame = 0;
                }

                c.z += c.vz;
                c.vz -= 1;
                if (c.z <= 0) {
                    c.z = 0;
                    c.vz = 0;
                } else {
                    frame = 0;
                }
            }

            if (c.y - c.height / 2 < canvas.height) {
                drawSprite(sprite.person, c.x - c.width / 2, (c.y - c.z) - c.height / 2, c.width, c.height, frame, 9, 0, 4);
            }
        }
    }

    for (var i = customers.length - 1; i > -1; --i) {
        var c = customers[i];
        c.time += 0.3;
        c.width = c.height = canvas.height * 0.05;

        var time = (Date.now() - c.start) / 1000;
        c.x += c.speed * (-0.05 * time + 0.35);
        c.y += 0.5 * Math.log(time * time + 1);

        if (c.y - c.height / 2 >= canvas.height) {
            customers.splice(i, 1);
            continue;
        }

        c.z += c.vz;
        c.vz -= 1;
        if (c.z <= 0) {
            c.z = 0;
            c.vz = 0;
        }

        drawSprite(sprite.person, c.x - c.width / 2, (c.y - c.z) - c.height / 2, c.width, c.height, 1 + (Math.floor(c.time) % 8), 9, 2, 4);
    }

    setTimeout(update, 17);
})();

function httpGet(url, state) {
    var x = new XMLHttpRequest();
    x.onreadystatechange = function() {
        state(x);
    };
    x.open("GET", url, true);
    x.send(null);
}

function resize() {
    var w = window.innerWidth - elFids.clientWidth;
    var h = window.innerHeight;
    var sw = w * 2;
    var sh = h * 2;

    for (var i = 0; i < retailers.length; ++i) {
        for (var e = 0; e < retailers[i].customers.length; ++e) {
            var c = retailers[i].customers[e];
            c.x = (c.x / canvas.width) * sw;
            c.y = (c.y / canvas.height) * sh;
        }
    }

    for (var i = 0; i < customers.length; ++i) {
        var c = customers[i];
        c.x = (c.x / canvas.width) * sw;
        c.y = (c.y / canvas.height) * sh;
    }

    canvas.width = sw;
    canvas.height = sh;
    canvas.style.width = w + "px";
    canvas.style.height = h + "px";
}

resize();
window.onresize = resize;
window.onkeydown = function(e) {
    if (e.key.toLowerCase() == "p") {
        var play = document.getElementById("play");
        if (play.classList.contains("hide")) {
            play.src = window.location.href.replace(/(view)(?!.*\/)/, "");
            play.classList.remove("hide");
        } else {
            play.src = "";
            play.classList.add("hide");
        }
        
    }
};
